// Package orderproj maintains a read model from the order events on the broker.
//
// It exists to make the consumer side of the outbox real rather than described. A projection
// is the canonical thing an outbox feeds, and -- more usefully for a template -- it is the
// case where duplicate delivery is VISIBLE: applying order.v1.OrderCreated twice increments
// the count twice, and the wrong number is still wrong tomorrow. That makes
// "deduplicate in the same transaction as the effect" demonstrable instead of advice.
//
// It is deliberately small. The point is the transaction boundary, not the read model.
package orderproj

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/example/gomicro/internal/order"
	"github.com/example/gomicro/internal/platform/events"
)

// Projection applies order events to the order_counts table.
type Projection struct {
	db *sql.DB

	// consumer namespaces the deduplication rows.
	//
	// processed_events is keyed by (consumer, message_id) so two consumers of the same
	// stream each get to process every message. Keying on the message alone would let
	// whichever ran first silently suppress the event for the other.
	consumer string

	log *slog.Logger
}

// New builds a projection.
func New(db *sql.DB, consumer string, log *slog.Logger) *Projection {
	return &Projection{db: db, consumer: consumer, log: log}
}

// Handle applies one event, exactly once.
//
// THE WHOLE POINT IS THE TRANSACTION. The deduplication row and the counter update commit
// together or not at all. Any arrangement where they are separate has a window: record first
// and crash, and the event is lost forever; apply first and crash, and it is applied twice on
// redelivery. Neither is recoverable after the fact, because nothing can tell afterwards which
// side of the window the crash fell on.
func (p *Projection) Handle(ctx context.Context, e events.Event) error {
	if e.Type != order.EventOrderCreated {
		// Not an error. A consumer that filters a whole subtree sees event types it has no
		// opinion about, and acking them is correct -- Nak-ing would redeliver them forever
		// and MaxDeliver would eventually dead-letter perfectly good events.
		return nil
	}

	var snapshot order.Order
	if err := json.Unmarshal(e.Payload, &snapshot); err != nil {
		// PERMANENT. The same bytes will fail to parse on every redelivery, so retrying five
		// times only delays the dead-letter by a minute and buries the real message.
		return events.Permanent(fmt.Errorf("decode %s payload: %w", e.Type, err))
	}

	tenant := e.TenantID
	if tenant == "" {
		tenant = snapshot.TenantID
	}
	if tenant == "" {
		return events.Permanent(errors.New("event carries no tenant, so it cannot be projected"))
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// CLAIM FIRST. ON CONFLICT DO NOTHING makes this the whole deduplication check: zero rows
	// affected means some earlier delivery already committed this event, and the right
	// response is to ack and move on.
	//
	// It also serialises concurrent workers. Two consumers handed the same message both
	// reach this INSERT, and the second blocks on the first's uncommitted row until it
	// commits -- at which point the second sees the conflict rather than double-counting.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO processed_events (consumer, message_id) VALUES ($1, $2)
		 ON CONFLICT (consumer, message_id) DO NOTHING`,
		p.consumer, e.ID)
	if err != nil {
		return fmt.Errorf("claim event %s: %w", e.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("claim event %s: %w", e.ID, err)
	}
	if affected == 0 {
		p.log.DebugContext(ctx, "event already applied; skipping",
			slog.String("event_id", e.ID), slog.String("consumer", p.consumer))
		return nil
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO order_counts (tenant_id, orders, last_order_at)
		 VALUES ($1, 1, $2)
		 ON CONFLICT (tenant_id) DO UPDATE
		   SET orders = order_counts.orders + 1,
		       last_order_at = GREATEST(order_counts.last_order_at, EXCLUDED.last_order_at)`,
		tenant, e.OccurredAt.UTC()); err != nil {
		return fmt.Errorf("project %s: %w", e.ID, err)
	}

	if err := tx.Commit(); err != nil {
		// Nothing was applied, so redelivery will do the whole thing again cleanly. This is
		// the failure mode the single transaction is FOR.
		return fmt.Errorf("commit projection of %s: %w", e.ID, err)
	}
	return nil
}

// Count returns the projected order count for a tenant. It exists for tests and for
// `cmd/worker` to log something meaningful at startup.
func (p *Projection) Count(ctx context.Context, tenantID string) (int64, error) {
	var n int64
	err := p.db.QueryRowContext(ctx,
		`SELECT orders FROM order_counts WHERE tenant_id = $1`, tenantID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read order count for %q: %w", tenantID, err)
	}
	return n, nil
}
