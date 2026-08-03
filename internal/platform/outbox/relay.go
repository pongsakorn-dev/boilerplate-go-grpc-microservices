package outbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// claimSQL is the whole reason this pattern works with more than one relay running.
//
// FOR UPDATE SKIP LOCKED: rows already locked by another transaction are passed over rather
// than waited for. Without SKIP LOCKED, a second relay blocks on the first one's rows and the
// two serialise -- no throughput from the second instance, only a second way to time out.
// Without FOR UPDATE at all, both relays read the same rows and publish every event twice.
//
// MEASURED against Postgres 17, one transaction holding rows 1-10 while a second issues the
// identical claim with a 400ms lock_timeout:
//
//	FOR UPDATE             the second transaction BLOCKS, then dies:
//	                       "canceling statement due to lock timeout (SQLSTATE 55P03)" at 403ms
//	FOR UPDATE SKIP LOCKED the second transaction returns rows 11-20 in 1ms
//
// That is the whole difference, and it is invisible in the RESULTS -- both arrangements
// eventually publish every row exactly once. Only the blocking distinguishes them, which is
// why relay_integration_test.go asserts that both relays reach Publish concurrently rather
// than asserting on what they published.
//
// ORDER BY id with the partial index on (id) WHERE published_at IS NULL means the claim is an
// index scan over exactly the unpublished rows, so it stays cheap as the table grows -- and
// an outbox table grows forever until something prunes it.
//
// AND failed_at IS NULL skips QUARANTINED rows -- see quarantine below. Without it, a row
// that can never be published is reclaimed on every drain and blocks everything behind it.
const claimSQL = `
SELECT id, tenant_id, aggregate_id, event_type, payload, occurred_at,
       coalesce(trace_parent, '')
FROM outbox
WHERE published_at IS NULL
  AND failed_at IS NULL
ORDER BY id
LIMIT $1
FOR UPDATE SKIP LOCKED`

// Relay moves rows from the outbox to a Publisher.
type Relay struct {
	db        *sql.DB
	publisher Publisher
	log       *slog.Logger

	batchSize int
}

// Option configures a Relay.
type Option func(*Relay)

// WithBatchSize bounds how many rows one drain claims.
//
// The bound matters more than the number. The claim transaction stays open while every
// message in the batch is published, so batch size is directly how long a database
// transaction is held across network I/O -- see the note on DrainOnce.
func WithBatchSize(n int) Option {
	return func(r *Relay) {
		if n > 0 {
			r.batchSize = n
		}
	}
}

// NewRelay builds a relay over a plain *sql.DB.
//
// database/sql rather than GORM: this is one hand-written query whose exact locking clause is
// the entire point. An ORM would add a translation layer between the SQL that matters and the
// SQL that runs, for no benefit.
func NewRelay(db *sql.DB, publisher Publisher, log *slog.Logger, opts ...Option) *Relay {
	r := &Relay{db: db, publisher: publisher, log: log, batchSize: 100}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// DrainOnce claims and publishes at most one batch. It returns how many were published.
//
// THE TRANSACTION IS HELD ACROSS THE PUBLISH, and that is a real trade-off rather than an
// oversight.
//
// Holding it is what makes the failure mode safe: if the broker rejects a message, or the
// process dies mid-batch, the transaction rolls back, published_at stays NULL, and the rows
// are simply reclaimed by the next drain. Nothing is lost and nothing needs a reconciliation
// job. The cost is a database transaction open for the duration of the batch's network I/O,
// which bloats vacuum and inflates replication lag if the batch is large or the broker slow.
//
// That is why the batch is bounded and why the alternative is named here rather than left for
// someone to discover: a fork under real load should switch to a LEASE -- claim rows with a
// short UPDATE that stamps claimed_at, commit immediately, publish outside any transaction,
// then mark published. It needs lease expiry to recover from a crashed relay, which is
// exactly the complexity this version avoids while the volume does not demand it.
func (r *Relay) DrainOnce(ctx context.Context) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	// Rollback on every path that does not commit. A no-op after a successful Commit.
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, claimSQL, r.batchSize)
	if err != nil {
		return 0, fmt.Errorf("claim: %w", err)
	}

	var batch []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.TenantID, &m.AggregateID, &m.EventType, &m.Payload,
			&m.OccurredAt, &m.TraceParent); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan: %w", err)
		}
		batch = append(batch, m)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("claim rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close claim: %w", err)
	}

	if len(batch) == 0 {
		// Nothing to do. Commit rather than rollback so the empty transaction ends cleanly
		// and does not show up as an aborted transaction in Postgres statistics.
		return 0, tx.Commit()
	}

	for _, m := range batch {
		err := r.publisher.Publish(ctx, m)
		if err == nil {
			continue
		}

		// Abandon the WHOLE batch, deliberately.
		//
		// Marking the ones that succeeded would need a second transaction and would leave
		// the batch half-committed if that failed too. Rolling back republishes a few
		// messages the broker already accepted, which is exactly the duplicate the
		// at-least-once contract already requires consumers to handle. Trading a duplicate
		// for a lost event is always the right way round.
		//
		// Cheap in practice, too: those republished messages carry the same Nats-Msg-Id, so
		// the broker's deduplication window collapses them back into one stored message. The
		// simplicity of this rollback is bought by that window existing.
		if !IsPermanent(err) {
			return 0, fmt.Errorf("publish outbox row %d: %w", m.ID, err)
		}

		// A PERMANENT failure gets the row out of the way.
		//
		// Without this the relay reclaims the same unpublishable row on every drain and
		// never reaches anything behind it -- one oversized payload silently stopping every
		// event in the service. The rollback happens first (deferred above) so the quarantine
		// runs in its own transaction against rows nobody holds a lock on.
		_ = tx.Rollback()
		if qErr := r.quarantine(ctx, m, err); qErr != nil {
			return 0, fmt.Errorf("publish outbox row %d failed permanently (%v) and it could "+
				"not be quarantined: %w", m.ID, err, qErr)
		}
		return 0, fmt.Errorf("quarantined outbox row %d: %w", m.ID, err)
	}

	if err := r.markPublished(ctx, tx, batch); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		// The messages ARE published; only the bookkeeping failed. They will be republished
		// on the next drain, which is the at-least-once contract doing its job rather than a
		// bug -- but it is worth a WARN, because a persistent commit failure means a
		// permanently duplicating relay.
		r.log.WarnContext(ctx, "outbox batch published but not marked; it will be republished",
			slog.Int("batch", len(batch)), slog.String("error", err.Error()))
		return 0, fmt.Errorf("commit: %w", err)
	}

	return len(batch), nil
}

// quarantine sets one row aside so the relay can move past it.
//
// The row is NOT deleted and NOT marked published: it stays in the table with the reason
// attached, invisible to the claim query, and an operator who fixes the cause replays it with
//
//	UPDATE outbox SET failed_at = NULL, failure_reason = NULL WHERE id = ...;
//
// An event is never discarded, which is the guarantee the whole pattern exists to make. The
// cost is that quarantined rows need watching -- a count of them is the alert worth having,
// because nothing else in the system will mention them again.
func (r *Relay) quarantine(ctx context.Context, m Message, cause error) error {
	// context.WithoutCancel: quarantining is what happens on the way to giving up, and it
	// must still run when the drain is being cancelled. Otherwise a relay shut down at the
	// wrong moment restarts and hits the same poison row forever.
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	res, err := r.db.ExecContext(qctx,
		`UPDATE outbox SET failed_at = now(), failure_reason = $2 WHERE id = $1 AND published_at IS NULL`,
		m.ID, cause.Error())
	if err != nil {
		return fmt.Errorf("quarantine outbox row %d: %w", m.ID, err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		// Nothing to do: another relay published it between the failure and this update.
		return nil
	}

	r.log.ErrorContext(ctx, "outbox row quarantined; it will not be retried until an operator clears failed_at",
		slog.Int64("outbox_id", m.ID),
		slog.String("event_type", m.EventType),
		slog.String("tenant_id", m.TenantID),
		slog.String("error", cause.Error()))
	return nil
}

// markPublished stamps the claimed rows.
func (r *Relay) markPublished(ctx context.Context, tx *sql.Tx, batch []Message) error {
	ids := make([]int64, 0, len(batch))
	for _, m := range batch {
		ids = append(ids, m.ID)
	}

	// = ANY($1) with a Go slice, which pgx maps to a Postgres bigint[].
	//
	// One statement rather than one per row: a batch of 100 becomes a single round trip
	// instead of 100, inside a transaction whose duration is the thing being minimised.
	res, err := tx.ExecContext(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("mark published: %w", err)
	}

	// Verified rather than assumed. If the UPDATE matched fewer rows than were claimed, the
	// relay would loop forever republishing the difference, and the symptom -- a consumer
	// receiving the same event repeatedly -- points nowhere near this line.
	affected, err := res.RowsAffected()
	if err == nil && affected != int64(len(batch)) {
		return fmt.Errorf("marked %d rows published but claimed %d; the relay would republish "+
			"the difference on every drain", affected, len(batch))
	}
	return nil
}

// Run drains in a loop until ctx is cancelled.
//
// It polls rather than listening. LISTEN/NOTIFY would cut idle latency, at the cost of a
// dedicated connection per relay, a reconnect-and-resync path, and the subtlety that
// notifications are lost while disconnected -- so a poll is still needed underneath as the
// correctness backstop. A poll alone is the honest version of the same reliability.
func (r *Relay) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}

		published, err := r.DrainOnce(ctx)
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// Shutting down mid-drain is ordinary: the transaction rolls back and the rows
			// stay unpublished for the next process.
			return nil
		case err != nil:
			r.log.ErrorContext(ctx, "outbox drain failed", slog.String("error", err.Error()))
		case published > 0:
			r.log.DebugContext(ctx, "outbox drained", slog.Int("published", published))
		}

		// A FULL batch means there is probably more waiting, so go again immediately rather
		// than sleeping. Without this, a backlog of 10,000 rows with a 100-row batch and a
		// one-second poll takes 100 seconds to clear -- and the backlog is exactly when
		// latency matters most.
		if published == r.batchSize && err == nil {
			timer.Reset(0)
		} else {
			timer.Reset(interval)
		}
	}
}
