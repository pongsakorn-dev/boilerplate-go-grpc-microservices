//go:build integration

package outbox_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/example/gomicro/internal/platform/outbox"
)

// TestTheObserverReportsWhatTheRelayCannotSay covers the three numbers this exists for.
//
// Each corresponds to a failure that is otherwise invisible from outside the process:
// a quarantined row nothing will ever retry, a backlog growing faster than it drains, and an
// oldest row ageing because nothing is draining at all.
func TestTheObserverReportsWhatTheRelayCannotSay(t *testing.T) {
	db := newDB(t)
	now := time.Now()

	// INSERTION ORDER IS PART OF THE FIXTURE, and getting it wrong made an earlier version of
	// this test vacuous.
	//
	// The age query is `ORDER BY id LIMIT 1` over the pending set. If the pending rows are
	// inserted first they hold the lowest ids, so dropping the WHERE clause entirely returns
	// the same row and the assertion below cannot fail -- confirmed by dropping it and
	// watching this pass.
	//
	// Real tables do not look like that. Published and quarantined rows are the OLD ones, so
	// they carry the lowest ids, and an unfiltered query returns them. That is the order here:
	// the two rows that must be excluded are inserted first and are therefore exactly what a
	// missing filter would surface.
	insertOutbox(t, db, now.Add(-48*time.Hour), outboxState{published: true})
	insertOutbox(t, db, now.Add(-24*time.Hour), outboxState{quarantined: true})
	insertOutbox(t, db, now.Add(-time.Hour), outboxState{})
	insertOutbox(t, db, now.Add(-time.Minute), outboxState{})

	reg := prometheus.NewPedanticRegistry()
	observer := outbox.NewObserver(db, discard(), reg)

	if err := observer.Observe(context.Background()); err != nil {
		t.Fatalf("observe: %v", err)
	}

	if got := gaugeValue(t, reg, "gomicro_outbox_pending_rows"); got != 2 {
		t.Errorf("pending rows = %v, want 2 (published and quarantined rows are not pending)", got)
	}
	if got := gaugeValue(t, reg, "gomicro_outbox_quarantined_rows"); got != 1 {
		t.Errorf("quarantined rows = %v, want 1.\n\n"+
			"This is the number that says an event is sitting undelivered with nothing "+
			"coming back for it. At zero, nobody is told.", got)
	}

	// The oldest PENDING row is an hour old. The 24h quarantined row and the 48h published one
	// are both older and neither is a candidate: the relay will never claim them, so counting
	// them here would make a permanent condition look like a draining backlog.
	age := gaugeValue(t, reg, "gomicro_outbox_oldest_pending_age_seconds")
	if age < 3500 || age > 3700 {
		t.Errorf("oldest pending age = %vs, want about 3600.\n\n"+
			"If this is near 86400 the quarantined row is being counted as pending; if near "+
			"172800, the published one is.", age)
	}

	if gaugeValue(t, reg, "gomicro_outbox_last_observation_timestamp_seconds") == 0 {
		t.Error("the last-observation timestamp was never set, so nothing can alert on this " +
			"observer having died -- and a dead observer leaves every gauge above frozen at a " +
			"healthy-looking value")
	}
}

// TestAnEmptyOutboxReportsZeroAge is the ordinary case, and it is a case rather than a
// footnote because SQL makes it easy to get wrong.
//
// With no pending rows the age query returns NO ROWS, not zero. Scanning that into a float
// errors, and an observer that returns an error on every tick of a perfectly healthy system
// would increment its failure counter forever and log a warning every fifteen seconds.
func TestAnEmptyOutboxReportsZeroAge(t *testing.T) {
	db := newDB(t)

	// A published row only: the table is not empty, but nothing is pending.
	insertOutbox(t, db, time.Now().Add(-time.Hour), outboxState{published: true})

	reg := prometheus.NewPedanticRegistry()
	observer := outbox.NewObserver(db, discard(), reg)

	if err := observer.Observe(context.Background()); err != nil {
		t.Fatalf("observing a drained outbox returned an error: %v\n\n"+
			"This is the healthy steady state. An observer that fails here fails on every "+
			"tick of every healthy service.", err)
	}

	if got := gaugeValue(t, reg, "gomicro_outbox_oldest_pending_age_seconds"); got != 0 {
		t.Errorf("oldest pending age = %v with nothing pending, want 0", got)
	}
	if got := gaugeValue(t, reg, "gomicro_outbox_pending_rows"); got != 0 {
		t.Errorf("pending rows = %v, want 0", got)
	}
}

// TestTheObserverKeepsReportingWhileTheRelayIsStuck is the design assertion, and the reason
// this is a separate goroutine rather than a step in the relay's loop.
//
// A relay wedged on a broker that accepts connections and never acks is the failure the
// oldest-pending-age gauge exists to catch. If the gauge were refreshed by the relay, that
// same wedge would stop it updating -- and a frozen gauge reads on a dashboard as a stable,
// healthy system. The metric would go quiet at exactly the moment it had something to say.
//
// Here the relay never runs at all, which is the limit case of being stuck: the age must climb
// anyway, because the observer's clock is its own.
func TestTheObserverKeepsReportingWhileTheRelayIsStuck(t *testing.T) {
	db := newDB(t)

	insertOutbox(t, db, time.Now().Add(-time.Hour), outboxState{})

	reg := prometheus.NewPedanticRegistry()
	observer := outbox.NewObserver(db, discard(), reg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = observer.Run(ctx, 50*time.Millisecond)
	}()

	// Run observes once immediately, so the first value is available without waiting a tick.
	// (That immediate observation is itself deliberate: without it a scrape inside the first
	// interval reports zero, which is indistinguishable from a healthy empty outbox.)
	first := waitForGauge(t, reg, "gomicro_outbox_oldest_pending_age_seconds", 2*time.Second)
	if first < 3500 {
		t.Fatalf("the first observation reported an age of %vs, want about 3600", first)
	}

	// Let several ticks pass. Nothing drains the outbox in the meantime.
	time.Sleep(300 * time.Millisecond)

	second := gaugeValue(t, reg, "gomicro_outbox_oldest_pending_age_seconds")
	if second <= first {
		t.Errorf("the age gauge did not advance while nothing was draining (%vs then %vs).\n\n"+
			"It is not tracking real time, so a relay that stops making progress produces a "+
			"flat line rather than a rising one -- and a flat line is what a healthy system "+
			"looks like.", first, second)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("the observer did not stop when its context was cancelled")
	}
}

// TestObservationFailuresAreCountedNotFatal covers the failure mode that must not take the
// worker down with it.
//
// The observer shares a process with the relay, whose job is delivering events. A database
// blip must not turn "metrics are briefly unavailable" into "events stopped flowing", so the
// failure is counted and logged rather than returned.
func TestObservationFailuresAreCountedNotFatal(t *testing.T) {
	db := newDB(t)

	reg := prometheus.NewPedanticRegistry()
	observer := outbox.NewObserver(db, discard(), reg)

	// Close the pool out from under it: every query now fails, which is what a database
	// outage looks like from here.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- observer.Run(ctx, 20*time.Millisecond) }()

	// The counter must climb rather than the goroutine dying.
	deadline := time.Now().Add(3 * time.Second)
	var failures float64
	for time.Now().Before(deadline) {
		failures = gaugeValue(t, reg, "gomicro_outbox_observation_failures_total")
		if failures > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if failures == 0 {
		t.Error("a failing observation did not increment the failure counter, so a " +
			"persistently broken observer is indistinguishable from a healthy one")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v; a database outage must not stop the worker, whose "+
				"actual job is draining the outbox", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the observer did not stop when its context was cancelled")
	}
}

// TestObserverQueriesUseTheirIndexes keeps the cost proportionate.
//
// This runs every OUTBOX_OBSERVE_INTERVAL forever. A sequential scan of the outbox on that
// schedule would cost more than the conditions it watches for, and would get worse exactly as
// the table grows -- so the health check becomes a load problem of its own.
func TestObserverQueriesUseTheirIndexes(t *testing.T) {
	db := newDB(t)
	now := time.Now()

	// A large table of PUBLISHED rows -- the normal shape -- with a handful pending and one
	// quarantined. Both queries must reach their tiny working set without walking the rest.
	for range 2000 {
		insertOutbox(t, db, now.Add(-48*time.Hour), outboxState{published: true})
	}
	for range 5 {
		insertOutbox(t, db, now.Add(-time.Minute), outboxState{})
	}
	insertOutbox(t, db, now.Add(-time.Hour), outboxState{quarantined: true})

	if _, err := db.Exec("ANALYZE outbox"); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	for _, tc := range []struct{ name, query string }{
		{
			name:  "pending count",
			query: "SELECT count(*) FROM outbox WHERE published_at IS NULL AND failed_at IS NULL",
		},
		{
			name: "oldest pending",
			query: "SELECT occurred_at FROM outbox WHERE published_at IS NULL AND failed_at IS NULL " +
				"ORDER BY id LIMIT 1",
		},
		{
			name:  "quarantined count",
			query: "SELECT count(*) FROM outbox WHERE failed_at IS NOT NULL",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := explain(t, db, tc.query)
			if !containsAny(plan, "Index Scan", "Index Only Scan", "Bitmap Index Scan") {
				t.Errorf("the %q query scans the whole table:\n%s\n\n"+
					"It runs on a timer forever, so this is a recurring full-table read that "+
					"grows with the table it is meant to be watching.", tc.name, plan)
			}
		})
	}
}

// --- helpers ---

// gaugeValue reads one metric out of a registry by name.
//
// Gathering rather than reaching into the collector: this asserts on what a Prometheus scrape
// would actually see, including the metric NAME. A renamed metric silently breaks every alert
// built on it, and reading the Go field directly would not notice.
func gaugeValue(t *testing.T, reg prometheus.Gatherer, name string) float64 {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			switch {
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue()
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue()
			}
		}
	}

	var have []string
	for _, f := range families {
		have = append(have, f.GetName())
	}
	t.Fatalf("no metric named %q was exported; found: %s", name, strings.Join(have, ", "))
	return 0
}

func waitForGauge(t *testing.T, reg prometheus.Gatherer, name string, timeout time.Duration) float64 {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if v := gaugeValueOrZero(reg, name); v != 0 {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never became non-zero within %s", name, timeout)
	return 0
}

func gaugeValueOrZero(reg prometheus.Gatherer, name string) float64 {
	families, err := reg.Gather()
	if err != nil {
		return 0
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if m.GetGauge() != nil {
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}
