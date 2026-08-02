// Command worker drains the transactional outbox to the message broker.
//
// A SEPARATE PROCESS from the API server, and the separation is the point.
//
// Running the relay inside orderd would tie publishing throughput to however many API
// replicas the HPA happens to have chosen -- so a traffic lull that scales the API down also
// slows event delivery, and scaling the API up for latency reasons multiplies the relay's
// database load for no reason. They have different bottlenecks and want different replica
// counts.
//
// It also keeps the failure domains apart: a relay stuck on an unavailable broker must not
// consume connections or memory in the process serving customer requests.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/jackc/pgx/v5/stdlib"

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
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	if cfg.Postgres.DSN.Reveal() == "" {
		return fmt.Errorf("POSTGRES_DSN is required: the outbox lives in the database")
	}

	log := observability.NewLogger(cfg, os.Stdout)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// database/sql, not GORM. The relay is one hand-written query whose locking clause is the
	// entire point; an ORM would add a layer between the SQL that matters and the SQL that
	// runs.
	db, err := sql.Open("pgx", cfg.Postgres.DSN.Reveal())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()

	// The relay's pool is small on purpose. It needs one connection per concurrent drain, and
	// a worker that opens as many connections as the API server would eat the database's
	// connection budget for work that is entirely background.
	db.SetMaxOpenConns(cfg.Outbox.MaxConns)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(cfg.Postgres.ConnMaxLifetime)

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}

	// LogPublisher is a PLACEHOLDER, and it says so on every event it handles.
	//
	// M8b replaces this one line with the JetStream publisher. Everything else -- claiming,
	// batching, the marking transaction, at-least-once semantics -- is already the real
	// thing, and none of it changes when the broker arrives.
	publisher := outbox.LogPublisher{Log: log}

	relay := outbox.NewRelay(db, publisher, log, outbox.WithBatchSize(cfg.Outbox.BatchSize))

	log.Info("outbox relay starting",
		"batch_size", cfg.Outbox.BatchSize,
		"poll_interval", cfg.Outbox.PollInterval.String(),
		"publisher", "log (placeholder until M8b wires JetStream)")

	if err := relay.Run(ctx, cfg.Outbox.PollInterval); err != nil {
		return fmt.Errorf("relay: %w", err)
	}

	// Run returns nil only on cancellation, so reaching here means SIGTERM. Anything the
	// relay had claimed rolled back with its transaction and stays unpublished for whichever
	// process picks it up next -- which is why a relay needs no drain period of its own.
	log.Info("outbox relay stopped")
	return nil
}
