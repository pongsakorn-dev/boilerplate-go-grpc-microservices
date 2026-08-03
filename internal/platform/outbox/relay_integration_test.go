//go:build integration

package outbox_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

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

// newDB gives each test its own migrated database.
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

// seed inserts n unpublished outbox rows.
func seed(t *testing.T, db *sql.DB, n int) {
	t.Helper()

	for i := range n {
		_, err := db.ExecContext(context.Background(), `
			INSERT INTO outbox (tenant_id, aggregate_id, event_type, payload, occurred_at)
			VALUES ($1, gen_random_uuid(), $2, $3, now())`,
			"tenant-a", "order.v1.OrderCreated", fmt.Sprintf(`{"seq":%d}`, i))
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

func unpublishedCount(t *testing.T, db *sql.DB) int {
	t.Helper()

	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count unpublished: %v", err)
	}
	return n
}

// recorder captures what was published, and can be told to fail.
type recorder struct {
	mu   sync.Mutex
	seen []outbox.Message

	failOn int64 // publish fails when this outbox id is reached; 0 disables
	block  chan struct{}

	// entered counts arrivals at Publish BEFORE the gate is waited on. It is what makes
	// blocking observable: a relay whose claim query is stuck never gets here.
	entered int
}

func (r *recorder) Publish(_ context.Context, m outbox.Message) error {
	r.mu.Lock()
	r.entered++
	r.mu.Unlock()

	if r.block != nil {
		<-r.block
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.failOn != 0 && m.ID == r.failOn {
		return errors.New("broker rejected the message")
	}
	r.seen = append(r.seen, m)
	return nil
}

func (r *recorder) enteredCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entered
}

func (r *recorder) ids() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]int64, 0, len(r.seen))
	for _, m := range r.seen {
		out = append(out, m.ID)
	}
	return out
}

// TestDrainPublishesAndMarks is the happy path, and it also settles an unverified assumption:
// that `id = ANY($1)` with a Go []int64 works through pgx's database/sql driver.
func TestDrainPublishesAndMarks(t *testing.T) {
	db := newDB(t)
	seed(t, db, 5)

	rec := &recorder{}
	relay := outbox.NewRelay(db, rec, discard())

	published, err := relay.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if published != 5 {
		t.Errorf("published %d, want 5", published)
	}
	if got := len(rec.seen); got != 5 {
		t.Errorf("the publisher saw %d messages, want 5", got)
	}
	if n := unpublishedCount(t, db); n != 0 {
		t.Errorf("%d rows are still unpublished after a successful drain; they would be "+
			"republished forever", n)
	}

	// A second drain finds nothing.
	published, err = relay.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("second DrainOnce: %v", err)
	}
	if published != 0 {
		t.Errorf("the second drain published %d rows; marking did not stick", published)
	}
}

// TestConcurrentRelaysDoNotBlockOnEachOther is the assertion NO fake can make, and it took
// two attempts to write correctly.
//
// The first version gave both relays a batch large enough for the whole table, then asserted
// no row was published twice. It passed -- and proved nothing. One relay claimed all 40 rows
// and the other claimed none, which is the SAME OBSERVABLE OUTCOME with or without SKIP
// LOCKED: without it the second relay simply BLOCKS until the first commits and then finds
// nothing left. Identical results, completely different behaviour.
//
// What actually differs is whether the second relay's claim query returns at all while the
// first holds its locks. So the recorder counts arrivals at Publish, and the test holds both
// relays there: if SKIP LOCKED is doing its job, BOTH reach Publish concurrently with
// disjoint rows. Without it, only one does and the other is stuck in the database.
func TestConcurrentRelaysDoNotBlockOnEachOther(t *testing.T) {
	db := newDB(t)

	const total = 40
	const batch = 10
	seed(t, db, total)

	// One *sql.DB, two transactions. database/sql hands each BeginTx its own connection, and
	// Postgres row locks are per-TRANSACTION -- so this is two independent sessions
	// contending for the same rows, which is what the locking semantics under test act on.
	recA, recB := &recorder{}, &recorder{}

	// Both held at Publish, so their claim transactions genuinely overlap. Without the gate
	// the first relay commits before the second starts and there is no contention at all.
	gate := make(chan struct{})
	recA.block, recB.block = gate, gate

	relayA := outbox.NewRelay(db, recA, discard(), outbox.WithBatchSize(batch))
	relayB := outbox.NewRelay(db, recB, discard(), outbox.WithBatchSize(batch))

	var wg sync.WaitGroup
	var errA, errB error
	wg.Add(2)
	go func() { defer wg.Done(); _, errA = relayA.DrainOnce(context.Background()) }()
	go func() { defer wg.Done(); _, errB = relayB.DrainOnce(context.Background()) }()

	// Long enough for both claims to complete, if they can.
	time.Sleep(500 * time.Millisecond)

	enteredA, enteredB := recA.enteredCount(), recB.enteredCount()

	close(gate)
	wg.Wait()

	if errA != nil || errB != nil {
		t.Fatalf("drains failed: A=%v B=%v", errA, errB)
	}

	// THE ASSERTION THAT DISTINGUISHES SKIP LOCKED FROM PLAIN FOR UPDATE.
	if enteredA == 0 || enteredB == 0 {
		t.Fatalf("only one relay reached Publish while the other held row locks (A=%d, B=%d).\n\n"+
			"That is a relay BLOCKED on another's locks. Without SKIP LOCKED a second "+
			"instance buys no throughput -- it just waits, and under load it times out "+
			"instead of helping.", enteredA, enteredB)
	}

	// And they took disjoint rows: no event delivered twice.
	seen := map[int64]int{}
	for _, id := range append(recA.ids(), recB.ids()...) {
		seen[id]++
	}
	for id, count := range seen {
		if count > 1 {
			t.Errorf("outbox row %d was published %d times by concurrent relays.\n\n"+
				"Without FOR UPDATE both relays read the same rows and every event ships twice.",
				id, count)
		}
	}
	if len(seen) != 2*batch {
		t.Errorf("%d distinct rows were published, want %d (two disjoint batches of %d)",
			len(seen), 2*batch, batch)
	}
	if n := unpublishedCount(t, db); n != total-2*batch {
		t.Errorf("%d rows remain unpublished, want %d", n, total-2*batch)
	}

	t.Logf("relay A published %d rows, relay B published %d, both claimed concurrently",
		len(recA.ids()), len(recB.ids()))
}

// TestAFailedPublishAbandonsTheWholeBatch keeps the failure direction safe.
//
// Nothing may be marked published when the broker rejected part of the batch: rolling back
// republishes a few messages the broker already accepted, which the at-least-once contract
// already requires consumers to handle. Trading a duplicate for a LOST event is never right.
func TestAFailedPublishAbandonsTheWholeBatch(t *testing.T) {
	db := newDB(t)
	seed(t, db, 5)

	// Find the third row's id and fail on it.
	var thirdID int64
	if err := db.QueryRowContext(context.Background(),
		`SELECT id FROM outbox ORDER BY id OFFSET 2 LIMIT 1`).Scan(&thirdID); err != nil {
		t.Fatalf("find third id: %v", err)
	}

	rec := &recorder{failOn: thirdID}
	relay := outbox.NewRelay(db, rec, discard())

	if _, err := relay.DrainOnce(context.Background()); err == nil {
		t.Fatal("a failed publish reported success")
	}

	if n := unpublishedCount(t, db); n != 5 {
		t.Errorf("%d of 5 rows remain unpublished after a mid-batch failure, want all 5.\n\n"+
			"Marking part of a failed batch loses the rest.", n)
	}
}

// TestBatchSizeIsRespected keeps the claim bounded.
//
// The claim transaction stays open across the batch's publishing, so an unbounded batch is an
// unbounded transaction held across network I/O -- which bloats vacuum and inflates
// replication lag.
func TestBatchSizeIsRespected(t *testing.T) {
	db := newDB(t)
	seed(t, db, 25)

	rec := &recorder{}
	relay := outbox.NewRelay(db, rec, discard(), outbox.WithBatchSize(10))

	published, err := relay.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if published != 10 {
		t.Errorf("one drain published %d rows with a batch size of 10", published)
	}
	if n := unpublishedCount(t, db); n != 15 {
		t.Errorf("%d rows remain unpublished, want 15", n)
	}
}

// TestRunDrainsABacklogAndStops covers the loop, including that a full batch is followed
// immediately rather than after a poll interval.
//
// Without that, a backlog of 10,000 rows at 100 per batch and a one-second poll takes 100
// seconds to clear -- and a backlog is exactly when latency matters most.
func TestRunDrainsABacklogAndStops(t *testing.T) {
	db := newDB(t)
	seed(t, db, 50)

	rec := &recorder{}
	relay := outbox.NewRelay(db, rec, discard(), outbox.WithBatchSize(10))

	// A poll interval far longer than the test. Reaching zero unpublished proves the relay
	// went again immediately after each full batch instead of waiting.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- relay.Run(ctx, time.Hour) }()

	deadline := time.After(5 * time.Second)
	for {
		if unpublishedCount(t, db) == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the backlog did not clear: %d rows still unpublished.\n\n"+
				"A full batch must be followed immediately, not after the poll interval.",
				unpublishedCount(t, db))
		case <-time.After(20 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil on cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Run did not return after its context was cancelled; the worker would hang " +
			"on shutdown and be SIGKILLed")
	}
}

// permanentRecorder fails permanently on one row and records everything else.
type permanentRecorder struct {
	mu     sync.Mutex
	seen   []int64
	failOn int64
}

func (r *permanentRecorder) Publish(_ context.Context, m outbox.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if m.ID == r.failOn {
		// The shape of a real one: a payload above the NATS server's max_payload is
		// rejected identically on every attempt, forever.
		return outbox.Permanent(errors.New("nats: maximum payload exceeded"))
	}
	r.seen = append(r.seen, m.ID)
	return nil
}

func (r *permanentRecorder) published() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.seen...)
}

// TestAPermanentFailureDoesNotStopTheOutbox is the head-of-line block, reproduced and fixed.
//
// The relay abandons a whole batch when a publish fails, which is right when the failure is
// transient: a broker restart heals itself and the rows are reclaimed. It is a total outage
// when the failure is NOT transient. One event whose payload exceeds the broker's max_payload
// is rejected identically every time, so the relay reclaims it, fails, rolls back, and never
// reaches a single row behind it -- the whole service's event stream stopped by one message,
// with nothing in the logs but the same error repeating.
//
// Quarantine is the answer: set that row aside, keep going. Nothing is deleted, so the event
// is recoverable once someone fixes the cause.
func TestAPermanentFailureDoesNotStopTheOutbox(t *testing.T) {
	db := newDB(t)
	seed(t, db, 5)

	// Row 2 of 5 can never be published, so rows 3-5 sit behind it.
	rec := &permanentRecorder{failOn: 2}
	relay := outbox.NewRelay(db, rec, discard())
	ctx := context.Background()

	// First drain hits the poison row and quarantines it. Row 1 was published before the
	// failure but rolls back unmarked -- the deliberate whole-batch abandon.
	if _, err := relay.DrainOnce(ctx); err == nil {
		t.Fatal("DrainOnce reported success despite a permanent publish failure")
	}

	var (
		failedAt sql.NullTime
		reason   sql.NullString
	)
	if err := db.QueryRowContext(ctx,
		`SELECT failed_at, failure_reason FROM outbox WHERE id = 2`).Scan(&failedAt, &reason); err != nil {
		t.Fatalf("read row 2: %v", err)
	}
	if !failedAt.Valid {
		t.Fatal("the unpublishable row was not quarantined, so every drain from now on " +
			"reclaims it and nothing behind it is ever published")
	}
	if !strings.Contains(reason.String, "maximum payload exceeded") {
		t.Errorf("failure_reason = %q, want the cause recorded so an operator can act on it",
			reason.String)
	}

	// The second drain must now get PAST it.
	published, err := relay.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("the drain after quarantine still failed: %v", err)
	}
	if published != 4 {
		t.Errorf("published %d rows after quarantine, want the other 4", published)
	}

	got := rec.published()
	for _, want := range []int64{1, 3, 4, 5} {
		if !slices.Contains(got, want) {
			t.Errorf("row %d was never published; the poison row is still blocking it "+
				"(published: %v)", want, got)
		}
	}
	if slices.Contains(rec.published(), int64(2)) {
		// It legitimately appears once, on the attempt that failed; it must not be retried.
		var attempts int
		for _, id := range got {
			if id == 2 {
				attempts++
			}
		}
		t.Errorf("the quarantined row was published %d times after being set aside", attempts)
	}

	// NOTHING IS DISCARDED. The row is still there, still unpublished, waiting for an
	// operator to clear failed_at and let it through.
	var quarantined int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM outbox WHERE failed_at IS NOT NULL AND published_at IS NULL`).Scan(&quarantined); err != nil {
		t.Fatalf("count quarantined: %v", err)
	}
	if quarantined != 1 {
		t.Errorf("%d rows are quarantined, want 1 -- the event must be recoverable, not deleted",
			quarantined)
	}
}

// TestClearingFailedAtReplaysAQuarantinedRow proves the documented recovery actually works.
//
// The comment on Relay.quarantine tells an operator to clear failed_at. A recovery procedure
// nobody has run is a recovery procedure that does not work.
func TestClearingFailedAtReplaysAQuarantinedRow(t *testing.T) {
	db := newDB(t)
	seed(t, db, 1)
	ctx := context.Background()

	poison := &permanentRecorder{failOn: 1}
	if _, err := outbox.NewRelay(db, poison, discard()).DrainOnce(ctx); err == nil {
		t.Fatal("expected the drain to fail")
	}

	// The relay now has nothing to do at all.
	rec := &permanentRecorder{}
	relay := outbox.NewRelay(db, rec, discard())
	if n, err := relay.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("DrainOnce over a fully quarantined outbox = (%d, %v), want (0, nil)", n, err)
	}

	// The documented fix, verbatim.
	if _, err := db.ExecContext(ctx,
		`UPDATE outbox SET failed_at = NULL, failure_reason = NULL WHERE id = 1`); err != nil {
		t.Fatalf("clear the quarantine: %v", err)
	}

	if n, err := relay.DrainOnce(ctx); err != nil || n != 1 {
		t.Fatalf("after clearing failed_at, DrainOnce = (%d, %v), want (1, nil).\n\n"+
			"The recovery procedure in the quarantine comment does not work.", n, err)
	}
	if unpublishedCount(t, db) != 0 {
		t.Error("the replayed row was not marked published")
	}
}

// TestTheRelayCarriesTheTraceParent covers the middle link of trace propagation, which is the
// one with no other test standing behind it.
//
// orderpg's test proves the trace is WRITTEN; events' test proves a message carrying one is
// resumed by the consumer. Between them sits the claim query, and it is a plain SELECT column
// list -- exactly the kind of thing a later edit drops without any test noticing, because both
// neighbours keep passing. The trace would then vanish silently at the one hop where nothing
// else can recover it.
func TestTheRelayCarriesTheTraceParent(t *testing.T) {
	db := newDB(t)

	const traceParent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	_, err := db.Exec(`
		INSERT INTO outbox (tenant_id, aggregate_id, event_type, payload, occurred_at, trace_parent)
		VALUES ('acme', gen_random_uuid(), 'order.created', '{}'::jsonb, now(), $1)`, traceParent)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	rec := &recordingPublisher{}
	relay := outbox.NewRelay(db, rec, discard())

	if _, err := relay.DrainOnce(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	published := rec.published()
	if len(published) != 1 {
		t.Fatalf("the relay published %d messages, want 1", len(published))
	}
	if got := published[0].TraceParent; got != traceParent {
		t.Errorf("the relay published TraceParent %q, want %q.\n\n"+
			"The claim query is not selecting trace_parent, so the trace is dropped between "+
			"the row and the broker -- at the one hop where there is nothing left to recover "+
			"it from.", got, traceParent)
	}
}

// recordingPublisher captures what the relay handed onward.
type recordingPublisher struct {
	mu   sync.Mutex
	msgs []outbox.Message
}

func (p *recordingPublisher) Publish(_ context.Context, m outbox.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, m)
	return nil
}

func (p *recordingPublisher) published() []outbox.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]outbox.Message(nil), p.msgs...)
}
