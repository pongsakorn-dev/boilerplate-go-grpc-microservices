package outbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Observer publishes the outbox's health as Prometheus metrics.
//
// WHY THIS EXISTS AT ALL. Two failures in this subsystem are completely silent from outside.
// A quarantined row -- one the relay gave up on -- is skipped by every future drain and nothing
// mentions it again, so an event sits undelivered until a human happens to look. And a relay
// wedged on a broker that accepts connections but never acks keeps running, keeps logging
// nothing, and keeps its process healthy while the backlog grows without bound.
//
// Neither is visible in the process. Both are one query away in the database.
//
// WHY IT DOES NOT RUN INSIDE THE RELAY'S LOOP, which was the obvious first design and is
// wrong. The relay's own poll is not a safe clock for these numbers: if the relay is wedged --
// the exact condition the oldest-pending-age gauge exists to detect -- it stops refreshing,
// and the gauge FREEZES at whatever it read last. A frozen gauge reads as a healthy steady
// state on a dashboard, and the one signal that would have named the problem is the one the
// problem suppresses.
//
// Running on its own ticker means a wedged relay leaves this goroutine untouched, so the age
// climbs and the alert fires. The independence is the feature.
type Observer struct {
	db  *sql.DB
	log *slog.Logger

	quarantined      prometheus.Gauge
	pending          prometheus.Gauge
	oldestPendingAge prometheus.Gauge
	failures         prometheus.Counter
	lastObservation  prometheus.Gauge
}

// NewObserver registers the outbox gauges on reg.
//
// A Registerer rather than the default registry, for the reason observability.Metrics gives:
// the default is process-global mutable state that any dependency can write to, and a
// duplicate registration panics.
func NewObserver(db *sql.DB, log *slog.Logger, reg prometheus.Registerer) *Observer {
	o := &Observer{
		db:  db,
		log: log,

		quarantined: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gomicro_outbox_quarantined_rows",
			Help: "Outbox rows with failed_at set. These are undelivered events that no future " +
				"drain will retry until an operator clears failed_at. Any value above zero is " +
				"an event nobody is coming back for.",
		}),
		pending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gomicro_outbox_pending_rows",
			Help: "Outbox rows waiting to be published. A healthy relay keeps this near zero " +
				"between polls; a sustained climb means publishing is slower than writing.",
		}),
		oldestPendingAge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gomicro_outbox_oldest_pending_age_seconds",
			Help: "Age of the oldest unpublished outbox row. THIS is what detects a wedged " +
				"relay -- unlike a liveness probe, it cannot be satisfied by a process that is " +
				"running but not draining.",
		}),
		failures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gomicro_outbox_observation_failures_total",
			Help: "Failed attempts to read outbox health from the database.",
		}),
		lastObservation: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gomicro_outbox_last_observation_timestamp_seconds",
			Help: "Unix time of the last successful observation. Alert on staleness here: if " +
				"this observer stops, every gauge above freezes at a value that looks healthy.",
		}),
	}

	reg.MustRegister(o.quarantined, o.pending, o.oldestPendingAge, o.failures, o.lastObservation)
	return o
}

// pendingSQL counts rows the relay has yet to publish.
//
// The predicate matches the claim query's exactly, so this counts what the relay would actually
// pick up and uses the same partial index (outbox_unpublished_idx). Quarantined rows are
// excluded because the relay will never claim them -- they are counted separately, and rolling
// them in here would make a permanent problem look like a temporary backlog.
const pendingSQL = `
SELECT count(*) FROM outbox
WHERE published_at IS NULL AND failed_at IS NULL`

// oldestPendingSQL reads the age of the row the relay will claim FIRST.
//
// ORDER BY id LIMIT 1 rather than min(occurred_at), and the difference matters under exactly
// the conditions this metric is for. An aggregate over the pending set has to walk all of it,
// so with a wedged relay and ten million pending rows the query gets expensive precisely when
// it is most needed. Taking the first index entry is one lookup at any backlog size.
//
// It also matches the relay's own ORDER BY id, so "oldest" here means "next to be published"
// rather than "earliest timestamp" -- which is the number an operator actually wants.
const oldestPendingSQL = `
SELECT EXTRACT(EPOCH FROM (now() - occurred_at)) FROM outbox
WHERE published_at IS NULL AND failed_at IS NULL
ORDER BY id
LIMIT 1`

// quarantinedSQL counts rows the relay has given up on. Served by outbox_quarantined_idx,
// which is partial and therefore usually empty.
const quarantinedSQL = `SELECT count(*) FROM outbox WHERE failed_at IS NOT NULL`

// Observe refreshes every gauge once.
func (o *Observer) Observe(ctx context.Context) error {
	var pending, quarantined int64

	if err := o.db.QueryRowContext(ctx, pendingSQL).Scan(&pending); err != nil {
		return fmt.Errorf("count pending outbox rows: %w", err)
	}
	if err := o.db.QueryRowContext(ctx, quarantinedSQL).Scan(&quarantined); err != nil {
		return fmt.Errorf("count quarantined outbox rows: %w", err)
	}

	// An EMPTY pending set is not an error, and it is the normal case.
	var age float64
	err := o.db.QueryRowContext(ctx, oldestPendingSQL).Scan(&age)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		age = 0
	case err != nil:
		return fmt.Errorf("read the oldest pending outbox row: %w", err)
	}

	o.pending.Set(float64(pending))
	o.quarantined.Set(float64(quarantined))
	o.oldestPendingAge.Set(age)
	o.lastObservation.SetToCurrentTime()

	return nil
}

// Run refreshes on a ticker until ctx is cancelled.
//
// It never returns an error for a failed observation. A database blip must not take down the
// worker, whose actual job -- draining the outbox -- has its own retry loop and its own
// tolerance for the same blip. The failure is counted instead, so a persistently failing
// observer is visible as a rising counter and a stale last-observation timestamp rather than
// as a crash loop that also stops the relay.
func (o *Observer) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 15 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Observe immediately so the metrics are populated before the first tick. Without this a
	// scrape in the first interval reports zero for everything, which is indistinguishable
	// from a healthy empty outbox -- including when there is a backlog of ten thousand rows.
	o.observeOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			o.observeOnce(ctx)
		}
	}
}

func (o *Observer) observeOnce(ctx context.Context) {
	// A bounded deadline per observation. Without it, one query blocked on a lock holds this
	// goroutine past every subsequent tick, and the metrics silently stop updating -- the
	// exact staleness this observer exists to prevent, caused by the observer.
	obsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := o.Observe(obsCtx); err != nil {
		if ctx.Err() != nil {
			return // shutting down; not a failure
		}
		o.failures.Inc()
		o.log.WarnContext(ctx, "could not read outbox health", slog.String("error", err.Error()))
	}
}
