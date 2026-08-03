//go:build integration

package outbox_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/example/gomicro/internal/platform/outbox"
)

// The prune is the only code in this repository whose job is to DELETE rows nobody asked it to
// delete, on a schedule, unsupervised. Every other destructive path in the system is driven by
// an explicit request.
//
// That asymmetry is why these tests are weighted the way they are: one case covers "the aged
// rows go", and the rest cover "everything else stays". A pruner that deletes too little is a
// table that grows; a pruner that deletes too much is an event nobody can ever recover.

// TestPruneKeepsQuarantinedRowsAtAnyAge is the assertion that matters most here.
//
// A quarantined row is the relay saying "I could not publish this and I am not going to try
// again until a human looks at it". It has failed_at set and published_at still NULL. Because
// it is never published and never retried, it is by construction the OLDEST row in the table --
// so it is the first thing any age-based sweep reaches.
//
// Deleting it would discard an event the outbox pattern exists to guarantee is never lost, and
// it would happen silently, to precisely the rows somebody was going to investigate.
//
// WHAT THIS TEST ACTUALLY CATCHES is the cutoff being applied to the wrong column. Removing
// `AND published_at IS NOT NULL` from the DELETE does not break it and cannot: `NULL < $1` is
// NULL, so unpublished rows are excluded by the comparison itself. Confirmed by removing that
// clause and watching this test still pass -- which is why the claim is written here rather
// than assumed.
//
// The mutation it does catch is `occurred_at < $1` in place of `published_at < $1`. That is the
// mistake worth guarding: occurred_at is the column that reads like "how old is this row", and
// sweeping on it deletes pending and quarantined events along with the published ones.
func TestPruneKeepsQuarantinedRowsAtAnyAge(t *testing.T) {
	db := newDB(t)
	now := time.Now()

	// A year old, and quarantined. Nothing about age should save it -- only its status.
	quarantined := insertOutbox(t, db, now.AddDate(-1, 0, 0), outboxState{quarantined: true})

	// A pending row of the same age: never published, never failed, still waiting for a relay
	// that has not got to it. Also not eligible, for the same reason.
	pending := insertOutbox(t, db, now.AddDate(-1, 0, 0), outboxState{})

	// And one that WAS published, to prove the pruner is running at all.
	published := insertOutbox(t, db, now.AddDate(-1, 0, 0), outboxState{published: true})

	pruner := outbox.NewPruner(db, discard(), 100)
	result, err := pruner.Prune(context.Background(), now, 24*time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	if result.Outbox != 1 {
		t.Errorf("prune deleted %d outbox rows, want exactly 1 (the published one)", result.Outbox)
	}

	if !rowExists(t, db, quarantined) {
		t.Error("THE QUARANTINED ROW WAS DELETED.\n\n" +
			"It carries an event that was never published and never will be until an operator " +
			"clears failed_at. Ageing it out discards the one thing in this table that someone " +
			"was going to act on, and nothing anywhere would report that it happened.\n\n" +
			"The DELETE must be restricted to published_at IS NOT NULL.")
	}
	if !rowExists(t, db, pending) {
		t.Error("an UNPUBLISHED row was deleted.\n\n" +
			"It is an event still waiting for the relay. Deleting it loses the event outright.")
	}
	if rowExists(t, db, published) {
		t.Error("the aged published row survived; the pruner is not deleting anything")
	}
}

// TestPruneRespectsTheCutoff checks the boundary in both directions.
//
// One-sided is not enough: a pruner that deletes everything passes "old rows go" perfectly.
func TestPruneRespectsTheCutoff(t *testing.T) {
	db := newDB(t)
	now := time.Now()

	const retention = 24 * time.Hour

	old := insertOutbox(t, db, now.Add(-retention-time.Hour), outboxState{published: true})
	recent := insertOutbox(t, db, now.Add(-retention+time.Hour), outboxState{published: true})

	oldDedup := insertProcessed(t, db, "c", "old", now.Add(-retention-time.Hour))
	recentDedup := insertProcessed(t, db, "c", "recent", now.Add(-retention+time.Hour))

	pruner := outbox.NewPruner(db, discard(), 100)
	result, err := pruner.Prune(context.Background(), now, retention, retention)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	if result.Outbox != 1 || result.ProcessedEvents != 1 {
		t.Errorf("prune reported outbox=%d processed_events=%d, want 1 and 1",
			result.Outbox, result.ProcessedEvents)
	}
	if rowExists(t, db, old) {
		t.Error("an outbox row past the cutoff survived")
	}
	if !rowExists(t, db, recent) {
		t.Error("an outbox row INSIDE the cutoff was deleted; the retention window is not honoured")
	}
	if processedExists(t, db, oldDedup) {
		t.Error("a processed_events row past the cutoff survived")
	}
	if !processedExists(t, db, recentDedup) {
		t.Error("a processed_events row INSIDE the cutoff was deleted.\n\n" +
			"That row is the consumer's record of having applied a message the broker can " +
			"still redeliver. Without it the event is applied a second time.")
	}
}

// TestPruneDeletesEverythingEligibleAcrossBatches catches the off-by-one that batching invites.
//
// The loop stops when a batch comes back short. Getting that wrong -- stopping on the first
// full batch, or on zero rather than on a short batch -- leaves rows behind on every run, so
// the table still grows, just more slowly. That is the failure this job would be least likely
// to be noticed for.
func TestPruneDeletesEverythingEligibleAcrossBatches(t *testing.T) {
	db := newDB(t)
	now := time.Now()

	// Deliberately not a multiple of the batch size, so the final batch is short.
	const total = 25
	const batch = 10

	for range total {
		insertOutbox(t, db, now.AddDate(0, 0, -30), outboxState{published: true})
	}

	pruner := outbox.NewPruner(db, discard(), batch)
	result, err := pruner.Prune(context.Background(), now, 24*time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	if result.Outbox != total {
		t.Errorf("prune deleted %d of %d eligible rows across batches of %d.\n\n"+
			"Rows are left behind on every run, so the table still grows -- slowly enough "+
			"that nobody notices until it is the largest one in the database.",
			result.Outbox, total, batch)
	}
	if n := countOutbox(t, db); n != 0 {
		t.Errorf("%d eligible rows remain after the prune", n)
	}
}

// TestEligibleCountsWithoutDeleting covers -dry-run, whose entire value is being harmless.
//
// It is the flag an operator reaches for before the first prune against a table nobody has
// ever pruned -- the one run that deletes months at once. A dry run that deletes is worse than
// no dry run at all, because it is used precisely when the person is being careful.
func TestEligibleCountsWithoutDeleting(t *testing.T) {
	db := newDB(t)
	now := time.Now()

	for range 3 {
		insertOutbox(t, db, now.AddDate(0, 0, -30), outboxState{published: true})
	}
	insertOutbox(t, db, now, outboxState{published: true})
	insertProcessed(t, db, "c", "aged", now.AddDate(0, 0, -30))

	pruner := outbox.NewPruner(db, discard(), 100)

	eligible, err := pruner.Eligible(context.Background(), now, 24*time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatalf("eligible: %v", err)
	}
	if eligible.Outbox != 3 || eligible.ProcessedEvents != 1 {
		t.Errorf("Eligible reported outbox=%d processed_events=%d, want 3 and 1",
			eligible.Outbox, eligible.ProcessedEvents)
	}

	if n := countOutbox(t, db); n != 4 {
		t.Errorf("the outbox holds %d rows after a DRY RUN, want 4.\n\n"+
			"Eligible deleted something. It is the flag used by someone being careful, so it "+
			"must not be the one that surprises them.", n)
	}
}

// TestPruneIsSafeOnEmptyTables covers the ordinary case: a CronJob firing hourly finds nothing
// to do almost every time.
func TestPruneIsSafeOnEmptyTables(t *testing.T) {
	db := newDB(t)

	pruner := outbox.NewPruner(db, discard(), 100)
	result, err := pruner.Prune(context.Background(), time.Now(), 24*time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatalf("prune on empty tables: %v", err)
	}
	if result.Outbox != 0 || result.ProcessedEvents != 0 {
		t.Errorf("prune reported %+v on empty tables", result)
	}
}

// TestPrunedQueriesUseTheirIndexes is why 00003_retention.sql exists.
//
// A retention job that sequentially scans the table it is bounding is a recurring full-table
// read, on a schedule, forever -- and it gets slower exactly as the table gets bigger, which is
// when it matters. "There is an index" and "the planner uses it" are different claims, so this
// asks the planner.
//
// THE DATA SHAPE HERE IS THE STEADY STATE, and getting it wrong made the first version of this
// test fail against a perfectly good index. When every row is eligible, a sequential scan IS
// the cheapest plan and Postgres is right to choose it -- an index that visits 100% of the
// heap is strictly more work. That shape only occurs once, on the first prune against a table
// nobody has ever pruned.
//
// Afterwards the table holds one retention window of rows and each run ages out a thin slice:
// with a 7-day window and an hourly job, well under 1% is eligible. That is the shape below,
// and it is the one the index exists for.
func TestPrunedQueriesUseTheirIndexes(t *testing.T) {
	db := newDB(t)
	now := time.Now()

	const (
		recent    = 2000
		aged      = 20
		retention = 24 * time.Hour
	)

	// The window: rows too recent to prune. Enough of them that a sequential scan is
	// expensive, because on a tiny table Postgres correctly ignores every index.
	//
	// SECONDS, NOT MINUTES. At one minute apart these 2000 rows would span 33 hours, so 559 of
	// them would fall past a 24-hour cutoff -- making 28% of the table eligible and the fixture
	// the opposite of the shape the comment above describes. Seconds keep the whole window
	// inside half an hour. (Written as minutes first; the assertion below is what would have
	// caught it, and is why the assertion exists rather than the comment alone.)
	for i := range recent {
		at := now.Add(-time.Duration(i) * time.Second)
		insertOutbox(t, db, at, outboxState{published: true})
		insertProcessed(t, db, "c", fmt.Sprintf("recent-%d", i), at)
	}
	// The thin slice that has aged out.
	for i := range aged {
		insertOutbox(t, db, now.AddDate(0, 0, -30), outboxState{published: true})
		insertProcessed(t, db, "c", fmt.Sprintf("aged-%d", i), now.AddDate(0, 0, -30))
	}
	if _, err := db.Exec("ANALYZE outbox; ANALYZE processed_events"); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	cutoff := now.Add(-retention)

	// THE FIXTURE ASSERTS ITS OWN SHAPE, because a data-shape comment is exactly the kind of
	// claim that rots without anything failing.
	//
	// Every planner assertion below is conditional on the eligible set being a thin slice: at
	// high selectivity a sequential scan IS the cheapest plan and Postgres is right to choose
	// one, so a drifted fixture would not fail here -- it would quietly stop testing the index
	// while still reporting PASS.
	var eligible int
	err := db.QueryRow(
		`SELECT count(*) FROM outbox WHERE published_at IS NOT NULL AND published_at < $1`,
		cutoff).Scan(&eligible)
	if err != nil {
		t.Fatalf("count eligible rows: %v", err)
	}
	if eligible != aged {
		t.Fatalf("%d of %d outbox rows are past the cutoff, want exactly %d.\n\n"+
			"The fixture is not the steady state it claims to be. At this selectivity a "+
			"sequential scan may legitimately be the cheapest plan, so the index assertions "+
			"below would pass or fail for reasons that have nothing to do with the index.",
			eligible, recent+aged, aged)
	}

	for _, tc := range []struct{ name, query, index string }{
		{
			name:  "outbox",
			query: "SELECT ctid FROM outbox WHERE published_at IS NOT NULL AND published_at < $1 LIMIT 1000",
			index: "outbox_published_idx",
		},
		{
			name:  "processed_events",
			query: "SELECT ctid FROM processed_events WHERE processed_at < $1 LIMIT 1000",
			index: "processed_events_processed_at_idx",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := explain(t, db, tc.query, cutoff)
			if !containsAny(plan, "Index Scan", "Index Only Scan", "Bitmap Index Scan") {
				t.Errorf("the %s prune query does not use an index:\n%s\n\n"+
					"It scans the whole table on every run of the job whose purpose is to stop "+
					"that table growing. Expected %s to be used.", tc.name, plan, tc.index)
			}
		})
	}
}

// --- helpers ---

type outboxState struct {
	published   bool
	quarantined bool
}

// insertOutbox writes one row with an explicit age and returns its id.
func insertOutbox(t *testing.T, db *sql.DB, at time.Time, state outboxState) int64 {
	t.Helper()

	var published, failed any
	if state.published {
		published = at
	}
	if state.quarantined {
		failed = at
	}

	var id int64
	err := db.QueryRow(`
		INSERT INTO outbox (tenant_id, aggregate_id, event_type, payload, occurred_at,
		                    published_at, failed_at, failure_reason)
		VALUES ('t', gen_random_uuid(), 'order.created', '{}'::jsonb, $1, $2, $3, $4)
		RETURNING id`,
		at, published, failed, quarantineReason(state)).Scan(&id)
	if err != nil {
		t.Fatalf("insert outbox row: %v", err)
	}
	return id
}

func quarantineReason(state outboxState) any {
	if state.quarantined {
		return "sabotage: a permanent publish failure"
	}
	return nil
}

func insertProcessed(t *testing.T, db *sql.DB, consumer, messageID string, at time.Time) string {
	t.Helper()

	_, err := db.Exec(
		`INSERT INTO processed_events (consumer, message_id, processed_at) VALUES ($1, $2, $3)`,
		consumer, messageID, at)
	if err != nil {
		t.Fatalf("insert processed_events row: %v", err)
	}
	return messageID
}

func rowExists(t *testing.T, db *sql.DB, id int64) bool {
	t.Helper()

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM outbox WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("check outbox row %d: %v", id, err)
	}
	return n == 1
}

func processedExists(t *testing.T, db *sql.DB, messageID string) bool {
	t.Helper()

	var n int
	err := db.QueryRow(`SELECT count(*) FROM processed_events WHERE message_id = $1`, messageID).Scan(&n)
	if err != nil {
		t.Fatalf("check processed_events row %q: %v", messageID, err)
	}
	return n == 1
}

func countOutbox(t *testing.T, db *sql.DB) int {
	t.Helper()

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM outbox`).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

// explain returns the planner's chosen plan as text.
func explain(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()

	rows, err := db.Query("EXPLAIN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v\nquery: %s", err, query)
	}
	defer func() { _ = rows.Close() }()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	return plan.String()
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// TestPruneReportsCancellationRatherThanSwallowingIt pins the exit-code contract.
//
// The first version returned nil whenever the context was done, which reads as tidy and hides
// the only signal that retention is failing. A prune that runs out of time has NOT done its
// job: the rows it could not reach are still there, and next run has more of them. Swallowing
// that makes a self-worsening backlog invisible, because a CronJob that exits 0 leaves no trace
// anywhere an operator looks.
//
// cmd/prune distinguishes the two cases -- Canceled exits 0, DeadlineExceeded exits non-zero --
// which it can only do if the error survives this far.
func TestPruneReportsCancellationRatherThanSwallowingIt(t *testing.T) {
	db := newDB(t)
	now := time.Now()

	insertOutbox(t, db, now.AddDate(0, 0, -30), outboxState{published: true})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before the first batch boundary

	pruner := outbox.NewPruner(db, discard(), 100)
	_, err := pruner.Prune(ctx, now, 24*time.Hour, 24*time.Hour)

	if err == nil {
		t.Fatal("a cancelled prune returned no error.\n\n" +
			"cmd/prune cannot then tell a job the cluster stopped from one that ran out of " +
			"time, and a retention job falling behind its own schedule exits 0 forever.")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled prune returned %v, want a wrapped context.Canceled", err)
	}

	// And it stopped at the boundary rather than part-way through a statement.
	if n := countOutbox(t, db); n != 1 {
		t.Errorf("the outbox holds %d rows, want the 1 that was never reached", n)
	}
}
