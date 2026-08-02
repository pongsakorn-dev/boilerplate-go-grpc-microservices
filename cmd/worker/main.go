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
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/example/gomicro/internal/order/orderproj"
	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/platform/events"
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
	// LoadWorker, not Load. The worker serves no RPCs and verifies no tokens, so the server's
	// auth rules do not apply to it -- and applying them stopped it booting with
	// APP_ENV=production at all. The role is declared by this binary rather than by an
	// environment variable, so a manifest cannot forget it. See config.Role.
	//
	// It does open ONE listener: the private admin surface below, which carries metrics and
	// authenticates nobody.
	cfg, err := config.LoadWorker()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
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

	// NATS_URL EMPTY IS A SUPPORTED CONFIGURATION, not a degraded one.
	//
	// LogPublisher prints exactly what would be published, so a fresh clone exercises the
	// entire outbox path -- claiming, batching, the marking transaction, at-least-once
	// semantics -- with no broker installed anywhere. Everything below this line is identical
	// either way; only the Publisher differs.
	var (
		publisher outbox.Publisher = outbox.LogPublisher{Log: log}
		consumer  *events.Consumer
		publishTo = "log (NATS_URL is empty; no broker)"
	)

	if cfg.NATS.URL != "" {
		p, closeNATS, err := events.Connect(ctx, cfg, log)
		if err != nil {
			return fmt.Errorf("nats: %w", err)
		}
		defer closeNATS()

		publisher = p
		publishTo = cfg.NATS.URL + " stream=" + cfg.NATS.Stream

		// The projection is the consumer's whole job. It applies each event and its
		// deduplication row in ONE transaction -- see internal/order/orderproj.
		consumer = events.NewConsumer(p.JetStream(), cfg.NATS,
			orderproj.New(db, cfg.NATS.Consumer, log), log)
	}

	relay := outbox.NewRelay(db, publisher, log, outbox.WithBatchSize(cfg.Outbox.BatchSize))

	// THE WORKER OPENS A LISTENER, and it did not used to. The reasoning is worth keeping.
	//
	// This binary shipped with no listener at all, on the argument that an HTTP server added
	// to answer a liveness probe only reports that the HTTP server is alive -- which is not
	// the question, since a relay wedged on a broker that accepts connections and never acks
	// passes such a probe forever. That argument was right and still is: there are NO PROBES
	// on the worker's pod, and deploy/k8s/base/worker.yaml says why at the point they would go.
	//
	// What changed is that the same file named the signal that DOES detect a wedged relay --
	// the age of the oldest unpublished row, and the count of quarantined ones -- and left
	// them in the database where nothing could scrape them. A number nobody can reach is not
	// an alert. This listener exists to carry those two, not to answer a probe.
	//
	// NewProcessRegistry rather than NewMetrics: the worker serves no RPCs, so a full set of
	// grpc_server_* series would be permanently zero here.
	registry := observability.NewProcessRegistry()
	observer := outbox.NewObserver(db, log, registry)

	adminSrv := observability.NewAdminServer(cfg, observability.NewAdminMux(registry, nil))

	log.Info("worker starting",
		"batch_size", cfg.Outbox.BatchSize,
		"poll_interval", cfg.Outbox.PollInterval.String(),
		"observe_interval", cfg.Outbox.ObserveInterval.String(),
		"admin_addr", cfg.AdminAddr,
		"publisher", publishTo,
		"consumer", consumer != nil)

	// The relay and the consumer are INDEPENDENT, and run concurrently.
	//
	// They are only in the same process because both are background work with the same
	// scaling story. Neither waits on the other: a consumer stuck on a slow projection must
	// not stop the relay from draining the outbox, or a downstream problem becomes an
	// upstream one.
	var wg sync.WaitGroup
	errs := make(chan error, 3)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := relay.Run(ctx, cfg.Outbox.PollInterval); err != nil {
			errs <- fmt.Errorf("relay: %w", err)
		}
	}()

	// The observer is a THIRD independent goroutine, not a step inside the relay's loop.
	//
	// That is the whole point of it: the gauge it maintains exists to detect a relay that has
	// stopped making progress, and a relay that has stopped making progress would also stop
	// refreshing a gauge it owned -- freezing it at a value that reads as healthy. See
	// outbox.Observer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := observer.Run(ctx, cfg.Outbox.ObserveInterval); err != nil {
			errs <- fmt.Errorf("observer: %w", err)
		}
	}()

	// The admin listener. ErrServerClosed is the ordinary shutdown path, not a failure.
	go func() {
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Reported, not fatal. Losing metrics is bad; taking the relay down with them --
			// so that events stop flowing because a scrape endpoint could not bind -- is
			// worse. A port collision must not become a delivery outage.
			log.Error("the admin listener stopped", "error", err.Error())
		}
	}()

	if consumer != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := consumer.Run(ctx); err != nil {
				errs <- fmt.Errorf("consumer: %w", err)
			}
		}()
	}

	wg.Wait()
	close(errs)

	// Stop the admin listener AFTER the workers have finished, so a final scrape during the
	// drain still sees real numbers. Its own context, because ctx is already cancelled by the
	// time this runs.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := adminSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("the admin listener did not shut down cleanly", "error", err.Error())
	}

	// Both return nil only on cancellation, so reaching here normally means SIGTERM. Anything
	// the relay had claimed rolled back with its transaction and stays unpublished for
	// whichever process picks it up next, which is why a relay needs no drain period of its
	// own; the consumer drains its in-flight messages inside Run.
	if err := <-errs; err != nil {
		return err
	}

	log.Info("worker stopped")
	return nil
}
