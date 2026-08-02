package outbox

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// Pruner deletes rows the system will never read again.
//
// Both tables this touches are append-only by design and neither is ever cleaned up by the
// code that writes them. That is correct -- the relay must not delete what it publishes, and
// the consumer must not delete its own dedup rows -- but it means table growth is unbounded
// until something outside both of them intervenes. This is that something.
//
// SEPARATE FROM THE RELAY, deliberately. Draining the outbox and pruning it have opposite
// urgency: a drain that stops means events stop flowing and someone should be paged, while a
// prune that stops means a table gets bigger and someone should look at it next week. Running
// the DELETE inside the relay's loop couples them, so a prune holding locks or generating WAL
// pressure turns a housekeeping problem into a delivery outage. cmd/prune runs it as its own
// process on its own schedule.
type Pruner struct {
	db        *sql.DB
	log       *slog.Logger
	batchSize int
}

// NewPruner builds a Pruner. A batchSize of zero or less takes the default.
func NewPruner(db *sql.DB, log *slog.Logger, batchSize int) *Pruner {
	if batchSize <= 0 {
		batchSize = 1000
	}
	return &Pruner{db: db, log: log, batchSize: batchSize}
}

// PruneResult reports what was removed.
type PruneResult struct {
	Outbox          int64
	ProcessedEvents int64
}

// pruneOutboxSQL deletes published rows past the cutoff, one bounded batch at a time.
//
// WHAT PROTECTS THE ROWS THAT MATTER IS WHICH COLUMN THE CUTOFF IS APPLIED TO -- published_at,
// not occurred_at. That is worth stating precisely, because the first version of this comment
// got it wrong in a way the tests then proved.
//
// A quarantined row -- one the relay gave up on -- has failed_at set and published_at still
// NULL, because it was never published. It stays in the table indefinitely waiting for an
// operator to clear failed_at, which makes it by construction the OLDEST row here and the first
// thing any age-based sweep reaches. Deleting it would discard an event the outbox exists to
// guarantee, quietly, from exactly the rows somebody was going to investigate.
//
// Ageing on published_at is what makes that impossible: a row that was never published has no
// published_at to compare, and in SQL `NULL < timestamp` is NULL, which WHERE treats as false.
// Pending and quarantined rows are therefore ineligible at any age, automatically.
//
// The consequence for reviewers: `AND published_at IS NOT NULL` below is REDUNDANT for
// correctness, and deleting it changes no behaviour -- measured, by deleting it and watching
// every test still pass. It is kept because it states the intent to a reader in the line rather
// than in this comment, and because it mirrors the partial index's own predicate. (Postgres
// matches outbox_published_idx either way; it proves NOT NULL from the comparison itself.)
//
// The sabotage that DOES turn the tests red is swapping published_at for occurred_at -- which
// is the realistic mistake, since occurred_at is the column that reads like "age".
//
// ctid rather than id: the subquery picks exactly batchSize physical rows and the outer DELETE
// addresses them directly, so neither statement needs a range and the batch size is honoured
// precisely.
const pruneOutboxSQL = `
DELETE FROM outbox
WHERE ctid = ANY(ARRAY(
    SELECT ctid FROM outbox
    WHERE published_at IS NOT NULL
      AND published_at < $1
    LIMIT $2
))`

// pruneProcessedEventsSQL deletes dedup rows past the cutoff.
//
// The caller is responsible for the cutoff being beyond the broker's own retention;
// config.Validate refuses a configuration where it is not. See RetentionConfig.ProcessedEvents
// for why that is a correctness boundary rather than a preference.
const pruneProcessedEventsSQL = `
DELETE FROM processed_events
WHERE ctid = ANY(ARRAY(
    SELECT ctid FROM processed_events
    WHERE processed_at < $1
    LIMIT $2
))`

// Prune removes aged rows from both tables and reports how many.
//
// now is passed in rather than read from the clock so a test can state the cutoff exactly
// instead of inserting rows with contrived timestamps and hoping.
func (p *Pruner) Prune(ctx context.Context, now time.Time, outboxAge, processedAge time.Duration) (PruneResult, error) {
	var result PruneResult

	outboxCutoff := now.Add(-outboxAge)
	deleted, err := p.deleteInBatches(ctx, "outbox", pruneOutboxSQL, outboxCutoff)
	result.Outbox = deleted
	if err != nil {
		return result, err
	}

	processedCutoff := now.Add(-processedAge)
	deleted, err = p.deleteInBatches(ctx, "processed_events", pruneProcessedEventsSQL, processedCutoff)
	result.ProcessedEvents = deleted
	if err != nil {
		return result, err
	}

	return result, nil
}

// Eligible counts what Prune WOULD delete, changing nothing.
//
// This exists because the first run of a retention job against a table nobody has ever pruned
// is the dangerous one: it is the only run that deletes months of rows at once, and an operator
// deserves to see that number before a CronJob discovers it at 03:00. cmd/prune -dry-run.
func (p *Pruner) Eligible(ctx context.Context, now time.Time, outboxAge, processedAge time.Duration) (PruneResult, error) {
	var result PruneResult

	err := p.db.QueryRowContext(ctx,
		`SELECT count(*) FROM outbox WHERE published_at IS NOT NULL AND published_at < $1`,
		now.Add(-outboxAge)).Scan(&result.Outbox)
	if err != nil {
		return result, fmt.Errorf("count eligible outbox rows: %w", err)
	}

	err = p.db.QueryRowContext(ctx,
		`SELECT count(*) FROM processed_events WHERE processed_at < $1`,
		now.Add(-processedAge)).Scan(&result.ProcessedEvents)
	if err != nil {
		return result, fmt.Errorf("count eligible processed_events rows: %w", err)
	}

	return result, nil
}

// deleteInBatches repeats one bounded DELETE until it stops finding rows.
//
// Each batch is its own implicit transaction. A prune interrupted halfway leaves the batches it
// completed deleted and the rest for next time, which is exactly right for a job whose work is
// idempotent and whose progress is worth keeping.
func (p *Pruner) deleteInBatches(ctx context.Context, table, query string, cutoff time.Time) (int64, error) {
	var total int64

	for {
		// Check for cancellation BEFORE each batch rather than relying on the query to notice.
		// A prune is interruptible work with no cleanup to do, so a stop mid-job should end it
		// at a batch boundary rather than in the middle of a DELETE.
		//
		// The context error is RETURNED rather than swallowed, and the distinction matters at
		// the call site: Canceled means the cluster stopped this job and the pod is going away
		// anyway, while DeadlineExceeded means the prune could not finish in its window -- which
		// is the signal that the schedule is too infrequent or the batch size too small for the
		// volume. cmd/prune exits 0 on the first and non-zero on the second. Returning nil for
		// both would hide the only evidence that retention is falling behind.
		if err := ctx.Err(); err != nil {
			p.log.InfoContext(ctx, "prune interrupted", slog.String("table", table),
				slog.Int64("deleted", total), slog.String("cause", err.Error()))
			return total, err
		}

		res, err := p.db.ExecContext(ctx, query, cutoff, p.batchSize)
		if err != nil {
			return total, fmt.Errorf("prune %s: %w", table, err)
		}

		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("prune %s: rows affected: %w", table, err)
		}
		total += n

		// A short batch means the table is drained past the cutoff. Comparing against
		// batchSize rather than zero saves one round trip per run.
		if n < int64(p.batchSize) {
			return total, nil
		}

		p.log.DebugContext(ctx, "prune batch", slog.String("table", table), slog.Int64("deleted", n))
	}
}
