// Command prune deletes rows the outbox and the consumer will never read again.
//
// A SEPARATE BINARY on a schedule, for the same reason cmd/migrate is one.
//
// The two tables it touches grow forever: the outbox keeps every event it has ever published,
// and processed_events keeps one row per message ever consumed. Neither is read again once its
// purpose is served, and neither the relay nor the consumer may delete its own rows -- the
// relay would be discarding what it just published, and the consumer would be forgetting what
// it has already applied.
//
// WHY NOT A GOROUTINE IN cmd/worker. Draining the outbox and pruning it have opposite urgency.
// A drain that stops means events are not being delivered and someone should be woken; a prune
// that stops means a table is larger than it should be and someone should look next week.
// Running both in one process couples those: a DELETE holding locks or generating WAL pressure
// would turn housekeeping into a delivery outage, and the worker's error budget would be spent
// on the wrong thing. In Kubernetes this is a CronJob (deploy/k8s/base/prune-cronjob.yaml).
//
// Run -dry-run first. The FIRST prune against a table nobody has ever pruned is the only one
// that deletes months of rows at once, and that number is worth seeing before a CronJob finds
// it at 03:00.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver

	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/platform/observability"
	"github.com/example/gomicro/internal/platform/outbox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	timeout := flag.Duration("timeout", 30*time.Minute,
		"overall deadline; a prune interrupted at a batch boundary keeps the batches it finished")
	dryRun := flag.Bool("dry-run", false,
		"count what would be deleted and change nothing")
	flag.Usage = usage
	flag.Parse()

	// LoadWorker, not Load: this binary opens no listener and verifies no tokens, so the
	// server's auth rules do not apply to it. It does need POSTGRES_DSN, which LoadWorker
	// requires. See config.Role.
	cfg, err := config.LoadWorker()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	log := observability.NewLogger(cfg, os.Stdout)

	// Interruptible at a batch boundary. A CronJob that overruns its window is stopped by
	// Kubernetes, and the right response is to keep the completed batches and exit cleanly
	// rather than to roll back an hour of progress.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	db, err := sql.Open("pgx", cfg.Postgres.DSN.Reveal())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()

	// One connection. This is a single-threaded batch job, and a pool sized for a server would
	// take connection budget from the replicas actually serving traffic.
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}

	pruner := outbox.NewPruner(db, log, cfg.Retention.BatchSize)
	now := time.Now()

	if *dryRun {
		eligible, err := pruner.Eligible(ctx, now, cfg.Retention.Outbox, cfg.Retention.ProcessedEvents)
		if err != nil {
			return err
		}
		fmt.Printf("dry run: would delete %d outbox rows and %d processed_events rows\n",
			eligible.Outbox, eligible.ProcessedEvents)
		fmt.Printf("  outbox:           published before %s\n",
			now.Add(-cfg.Retention.Outbox).Format(time.RFC3339))
		fmt.Printf("  processed_events: processed before %s\n",
			now.Add(-cfg.Retention.ProcessedEvents).Format(time.RFC3339))
		return nil
	}

	log.Info("prune starting",
		"outbox_retention", cfg.Retention.Outbox.String(),
		"processed_events_retention", cfg.Retention.ProcessedEvents.String(),
		"batch_size", cfg.Retention.BatchSize)

	result, err := pruner.Prune(ctx, now, cfg.Retention.Outbox, cfg.Retention.ProcessedEvents)

	// Report what was done even on failure. A prune that deleted 40,000 rows and then hit a
	// statement timeout has still done most of its job, and an operator needs the number.
	log.Info("prune finished",
		"outbox_deleted", result.Outbox,
		"processed_events_deleted", result.ProcessedEvents)

	// A CANCELLED PRUNE IS NOT A FAILED ONE, but an EXPIRED one is.
	//
	// Canceled means SIGTERM: the cluster is stopping this job, the pod is going away, and the
	// batches already committed are kept. Exiting non-zero there would fill the CronJob's
	// failure history with events that are not failures, and the noise would hide the case
	// below.
	//
	// DeadlineExceeded means the prune could not get through its backlog inside -timeout. That
	// is the one signal that retention is falling behind the write rate -- the schedule is too
	// infrequent, or RETENTION_BATCH_SIZE is too small -- and it must reach someone. It is
	// also self-worsening: every run that does not finish leaves more for the next one.
	if errors.Is(err, context.Canceled) {
		log.Info("prune stopped by a signal; the completed batches are kept")
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("prune did not finish within its deadline; the backlog is growing "+
			"faster than this schedule clears it -- run it more often or raise "+
			"RETENTION_BATCH_SIZE (deleted %d outbox and %d processed_events rows): %w",
			result.Outbox, result.ProcessedEvents, err)
	}

	return err
}

func usage() {
	fmt.Fprint(os.Stderr, `prune deletes aged rows from the outbox and processed_events.

Quarantined outbox rows (failed_at set) are NEVER deleted, at any age: they are events
the system has not yet delivered and has promised not to lose.

Usage:
  prune [flags]

Configuration comes from the environment, same as the service:
  POSTGRES_DSN                required
  RETENTION_OUTBOX            how long a published outbox row is kept (default 168h)
  RETENTION_PROCESSED_EVENTS  how long a dedup row is kept (default 192h)
                              MUST exceed NATS_STREAM_MAX_AGE, or a redelivered message
                              can be applied twice; startup refuses it otherwise
  RETENTION_BATCH_SIZE        rows per DELETE (default 1000)

Flags:
`)
	flag.PrintDefaults()
}
