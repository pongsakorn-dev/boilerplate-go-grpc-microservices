//go:build integration

package orderproj_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/example/gomicro/internal/order"
	"github.com/example/gomicro/internal/order/orderproj"
	"github.com/example/gomicro/internal/platform/events"
	"github.com/example/gomicro/internal/platform/outbox"
	"github.com/example/gomicro/internal/platform/testdb"
)

var instance *testdb.Instance

func TestMain(m *testing.M) {
	ctx := context.Background()

	inst, err := testdb.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb.Start: %v\n", err)
		os.Exit(m.Run())
	}
	instance = inst

	code := m.Run()
	_ = inst.Terminate(ctx)
	os.Exit(code)
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newDB(t *testing.T) *sql.DB {
	t.Helper()

	testdb.RequireDocker(t)
	if instance == nil {
		t.Skip("no database instance; see the TestMain output above")
	}

	db, err := sql.Open("pgx", instance.Fresh(t))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// event builds an order.v1.OrderCreated the way the publisher would deliver it.
func event(id, tenant string) events.Event {
	snapshot := order.Order{
		ID:         "11111111-2222-3333-4444-555555555555",
		TenantID:   tenant,
		CustomerID: "cust-1",
		CreatedAt:  time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC),
	}
	payload, _ := json.Marshal(snapshot)

	return events.Event{
		ID:          id,
		TenantID:    tenant,
		Type:        order.EventOrderCreated,
		AggregateID: snapshot.ID,
		Payload:     payload,
		OccurredAt:  snapshot.CreatedAt,
		Deliveries:  1,
	}
}

// TestARedeliveredEventIsAppliedOnce is the assertion the whole consumer design exists for.
//
// At-least-once delivery is the contract, so redelivery is normal, not exceptional: an ack
// that never lands, a handler that crashes after committing, a republished outbox batch
// arriving after the deduplication window. A projection that applies the same event twice
// produces a number that is silently wrong and stays wrong.
func TestARedeliveredEventIsAppliedOnce(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	p := orderproj.New(db, "order-projection", discard())

	e := event("orderd-1", "acme")
	for range 3 {
		if err := p.Handle(ctx, e); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}

	n, err := p.Count(ctx, "acme")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 1 {
		t.Errorf("order_counts.orders = %d after three deliveries of one event, want 1.\n\n"+
			"Deduplication and the counter update have to commit in the SAME transaction; "+
			"any arrangement where they are separate has a window that produces exactly this.", n)
	}
}

// TestDistinctEventsAreEachApplied is the other half. A deduplication key that is too coarse
// silently drops real events, and a projection that undercounts looks just as plausible as
// one that overcounts.
func TestDistinctEventsAreEachApplied(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	p := orderproj.New(db, "order-projection", discard())

	for i := range 4 {
		if err := p.Handle(ctx, event(fmt.Sprintf("orderd-%d", i), "acme")); err != nil {
			t.Fatalf("Handle %d: %v", i, err)
		}
	}
	// A different tenant must have its own row, not share acme's.
	if err := p.Handle(ctx, event("orderd-99", "globex")); err != nil {
		t.Fatalf("Handle globex: %v", err)
	}

	if n, _ := p.Count(ctx, "acme"); n != 4 {
		t.Errorf("acme count = %d, want 4", n)
	}
	if n, _ := p.Count(ctx, "globex"); n != 1 {
		t.Errorf("globex count = %d, want 1", n)
	}
}

// TestTwoWorkersHandedTheSameEventApplyItOnce is the case a single-process test cannot reach.
//
// Two replicas of cmd/worker exist for availability, and JetStream can deliver the same
// message to both -- after an AckWait expiry, or during a consumer rebalance. The INSERT ...
// ON CONFLICT is what serialises them: the second blocks on the first's uncommitted row and
// then sees the conflict, instead of reading "not processed yet" and double-counting.
//
// Only a real database can show this. A mutex-guarded fake proves the mutex works.
func TestTwoWorkersHandedTheSameEventApplyItOnce(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	const workers = 8
	e := event("orderd-7", "acme")

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		failed  []error
		barrier = make(chan struct{})
	)

	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := orderproj.New(db, "order-projection", discard())

			<-barrier // start together, so they genuinely contend
			if err := p.Handle(ctx, e); err != nil {
				mu.Lock()
				failed = append(failed, err)
				mu.Unlock()
			}
		}(i)
	}

	close(barrier)
	wg.Wait()

	for _, err := range failed {
		t.Errorf("a concurrent handler failed: %v", err)
	}

	p := orderproj.New(db, "order-projection", discard())
	n, err := p.Count(ctx, "acme")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 1 {
		t.Errorf("order_counts.orders = %d after %d workers handled the same event, want 1",
			n, workers)
	}
}

// TestTwoConsumersEachGetToProcessEveryEvent explains why processed_events is keyed by
// consumer as well as message.
//
// Keying on the message alone is the obvious simplification and it is a data-loss bug: the
// first consumer to run would suppress the event for every other consumer of the same stream,
// silently, and only the projections that were never built would notice.
func TestTwoConsumersEachGetToProcessEveryEvent(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	e := event("orderd-1", "acme")

	if err := orderproj.New(db, "order-projection", discard()).Handle(ctx, e); err != nil {
		t.Fatalf("first consumer: %v", err)
	}
	if err := orderproj.New(db, "billing-projection", discard()).Handle(ctx, e); err != nil {
		t.Fatalf("second consumer: %v", err)
	}

	var rows int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM processed_events WHERE message_id = 'orderd-1'`).Scan(&rows); err != nil {
		t.Fatalf("count processed_events: %v", err)
	}
	if rows != 2 {
		t.Errorf("processed_events holds %d rows for one message across two consumers, want 2",
			rows)
	}

	// Both applied it, so the counter moved twice -- correctly, because these are two
	// different read models that happen to share a table in this small example.
	if n, _ := orderproj.New(db, "x", discard()).Count(ctx, "acme"); n != 2 {
		t.Errorf("count = %d, want 2 (one per consumer)", n)
	}
}

// TestAnUnparseablePayloadIsPermanent keeps a poison message out of the retry budget.
func TestAnUnparseablePayloadIsPermanent(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	e := event("orderd-1", "acme")
	e.Payload = []byte(`{ this is not json`)

	err := orderproj.New(db, "order-projection", discard()).Handle(ctx, e)
	if err == nil {
		t.Fatal("an unparseable payload was accepted")
	}
	if !outbox.IsPermanent(err) {
		t.Errorf("the error is not permanent: %v\n\n"+
			"The same bytes fail identically on every redelivery, so retrying five times only "+
			"delays the dead letter and buries the real message.", err)
	}

	// And nothing was written -- a failed handler must leave no trace, or a later retry would
	// find itself already recorded and skip the work.
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM processed_events`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Errorf("a failed handler left %d rows in processed_events, so a retry would skip "+
			"the work it never did", rows)
	}
}

// TestUnknownEventTypesAreAckedRatherThanRetried covers the subtree the consumer does not care
// about.
//
// A consumer filtering a whole prefix sees types it has no opinion about. Nak-ing them would
// redeliver them forever and eventually dead-letter perfectly good events.
func TestUnknownEventTypesAreAckedRatherThanRetried(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	e := event("orderd-1", "acme")
	e.Type = "order.v1.OrderCancelled"

	if err := orderproj.New(db, "order-projection", discard()).Handle(ctx, e); err != nil {
		t.Fatalf("an unhandled event type returned an error: %v", err)
	}
	if n, _ := orderproj.New(db, "x", discard()).Count(ctx, "acme"); n != 0 {
		t.Errorf("count = %d, want 0 -- an unrelated event type must not be projected", n)
	}
}
