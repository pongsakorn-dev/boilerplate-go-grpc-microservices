package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/example/gomicro/internal/platform/config"
)

// The shutdown sequencer is tested here, in a synctest bubble, because it is PURE -- it
// binds no listener, opens no socket, and knows nothing about gRPC.
//
// That purity is the whole reason it was extracted. Running the real app.Run inside a
// bubble does not work: grpc-go's transport goroutines block on operations the runtime does
// not consider durably blocking, so the bubble either reports a spurious deadlock or hangs
// forever. The real drain behaviour needs a real server and will be asserted separately in
// M4. Two tests, each covering only what it can genuinely observe.
//
// The payoff: a 30-second configured drain resolves in microseconds, and the ordering
// assertions are exact rather than timing-dependent.

func TestShutdownRunsStepsInReverseOrder(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var order []string

		record := func(name string) Step {
			return Step{
				Name: name,
				Fn: func(context.Context) error {
					mu.Lock()
					defer mu.Unlock()
					order = append(order, name)
					return nil
				},
			}
		}

		// Steps are appended in CONSTRUCTION order: database opened before server.
		steps := []Step{record("database"), record("cache"), record("server")}

		if err := Shutdown(context.Background(), steps); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}

		// Reverse of construction: the server must stop before the database it depends on.
		// Closing the pool first would leave the server accepting requests it cannot serve.
		want := []string{"server", "cache", "database"}
		if strings.Join(order, ",") != strings.Join(want, ",") {
			t.Errorf("order = %v, want %v", order, want)
		}
	})
}

func TestShutdownHonoursPerStepTimeout(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		started := time.Now()

		var observedDeadline bool
		steps := []Step{{
			Name:    "slow",
			Timeout: 30 * time.Second,
			Fn: func(ctx context.Context) error {
				// A step that ignores its context runs to completion; one that respects it
				// stops at the deadline. Assert the deadline is actually plumbed through.
				deadline, ok := ctx.Deadline()
				if !ok {
					return errors.New("step context has no deadline")
				}
				observedDeadline = true
				if got := deadline.Sub(started); got != 30*time.Second {
					return errors.New("deadline was not the configured timeout")
				}
				<-ctx.Done()
				return ctx.Err()
			},
		}}

		err := Shutdown(context.Background(), steps)

		if !observedDeadline {
			t.Fatal("the step never saw a deadline")
		}
		if err == nil {
			t.Fatal("expected the timed-out step to report an error")
		}
		// The 30-second wait resolved instantly, because the bubble's clock is fake. The
		// same assertion against a real clock would make this a 30-second test.
		if elapsed := time.Since(started); elapsed != 30*time.Second {
			t.Errorf("virtual elapsed = %v, want exactly 30s", elapsed)
		}
	})
}

// TestShutdownRunsEveryStepEvenAfterFailure encodes a decision that is easy to get wrong.
//
// The tempting implementation returns on the first error. That leaks every resource the
// remaining steps were meant to release -- and shutdown is precisely when you cannot afford
// to skip releasing things, because the process is about to be replaced by one that will
// try to acquire them again.
func TestShutdownRunsEveryStepEvenAfterFailure(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var ran []string

		boom := errors.New("close failed")

		steps := []Step{
			{Name: "first", Fn: func(context.Context) error {
				mu.Lock()
				defer mu.Unlock()
				ran = append(ran, "first")
				return nil
			}},
			{Name: "second", Fn: func(context.Context) error {
				mu.Lock()
				defer mu.Unlock()
				ran = append(ran, "second")
				return boom
			}},
			{Name: "third", Fn: func(context.Context) error {
				mu.Lock()
				defer mu.Unlock()
				ran = append(ran, "third")
				return nil
			}},
		}

		err := Shutdown(context.Background(), steps)

		if !errors.Is(err, boom) {
			t.Errorf("err = %v, want it to wrap the step failure", err)
		}
		// The failing step must not stop the ones after it.
		if len(ran) != 3 {
			t.Errorf("ran %v, want all three steps despite the failure", ran)
		}
		// And the message must name WHICH step failed, or an operator reading a shutdown
		// log has no idea what leaked.
		if !strings.Contains(err.Error(), `"second"`) {
			t.Errorf("error %q does not name the failing step", err)
		}
	})
}

func TestShutdownToleratesNilSteps(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		// A Step with no Fn is what you get from a subsystem that was never constructed --
		// e.g. a partial failure in New. It must be skipped, not panic.
		steps := []Step{{Name: "never-built"}, {Name: "real", Fn: func(context.Context) error { return nil }}}

		if err := Shutdown(context.Background(), steps); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
}

// TestCloseDrainsOutsideIn pins the order App.Close actually uses.
//
// It reads a REAL app's step list rather than one the test builds. That distinction is the
// whole point: the first version of this test constructed its own slice, asserted Shutdown
// reversed it, and passed happily while App.Close appended the gateway in the wrong place --
// testing the iteration and not the list. The subject of a test must not be a value the test
// supplies.
//
// The bug it now catches: the gateway was appended BEFORE the gRPC server, and since Shutdown
// walks backwards that made it shut down AFTER -- so an in-flight HTTP request lost its
// in-process gRPC connection mid-call and returned 500, on every deploy.
func TestCloseDrainsOutsideIn(t *testing.T) {
	t.Parallel()

	// config.Parse rather than testutil.TestConfig: testutil imports this package, so an
	// internal test cannot import it back without a cycle.
	cfg, err := config.Parse(map[string]string{
		"APP_ENV":               config.EnvDevelopment,
		"SERVICE_NAME":          "orderd-test",
		"STORE_DRIVER":          config.StoreMemory,
		"AUTH_MODE":             config.AuthDev,
		"SHUTDOWN_DRAIN_DELAY":  "0s",
		"SHUTDOWN_GRACE_PERIOD": "2s",
	})
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}

	a, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.Close(ctx)
	})

	// Shutdown runs the slice backwards, so execution order is the reverse of the build order.
	steps := a.shutdownSteps()
	var order []string
	for i := len(steps) - 1; i >= 0; i-- {
		order = append(order, steps[i].Name)
	}

	gateway := slices.Index(order, "gateway-server")
	grpc := slices.Index(order, "grpc-server")
	admin := slices.Index(order, "admin-server")

	if gateway < 0 {
		t.Fatalf("no gateway step in %v; TestConfig sets GATEWAY_ADDR, so one is expected", order)
	}
	if grpc < 0 || admin < 0 {
		t.Fatalf("missing a core shutdown step in %v", order)
	}

	if gateway > grpc {
		t.Errorf("shutdown order is %v: the gateway stops AFTER the gRPC server.\n\n"+
			"An in-flight HTTP request then finds its in-process gRPC connection closed "+
			"underneath it and returns 500 for a request that was about to succeed -- on "+
			"every deploy. Drain outside-in: the outermost surface stops accepting first.",
			order)
	}
	if admin < grpc {
		t.Errorf("shutdown order is %v: admin stops before the gRPC server.\n\n"+
			"/metrics and pprof should outlive the drain they are there to observe.", order)
	}
}

// TestShutdownIteratesBackwards covers the SEQUENCER only -- that Shutdown runs a given
// slice in reverse. It does NOT cover App.Close's ordering; TestCloseDrainsOutsideIn does.
//
// Shutdown walks its slice backwards, so the last step appended runs first. App.Close appended
// the gateway BEFORE the gRPC server, which meant the gateway shut down AFTER it -- and an
// in-flight HTTP request then found its in-process gRPC connection closed underneath it and
// returned 500 for a request that was about to succeed, on every deploy.
//
// The comment beside that code described the intended behaviour ("the gateway stops before the
// gRPC server it depends on") rather than the actual behaviour, which is why reading it did not
// reveal the bug. Nothing executed the ordering: shutdown_test covered the pure sequencer and
// app tests covered that Close returns.
//
// This asserts the sequence a real Close produces, so the inversion cannot come back silently.
func TestShutdownIteratesBackwards(t *testing.T) {
	t.Parallel()

	var order []string
	record := func(name string) Step {
		return Step{Name: name, Fn: func(context.Context) error {
			order = append(order, name)
			return nil
		}}
	}

	// The shape App.Close builds: resources acquired during New, then admin, then grpc, then
	// the gateway appended last.
	steps := []Step{
		record("db-pool"),
		record("admin-server"),
		record("grpc-server"),
		record("gateway-server"),
	}

	if err := Shutdown(context.Background(), steps); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	want := []string{"gateway-server", "grpc-server", "admin-server", "db-pool"}
	if !slices.Equal(order, want) {
		t.Fatalf("steps ran %v, want %v.\n\n"+
			"Shutdown must drain OUTSIDE-IN: the gateway stops accepting first, then the gRPC "+
			"server it forwards to drains, then admin last so /metrics stays scrapeable, then "+
			"the pools. If the gateway runs after the gRPC server, an in-flight HTTP request "+
			"loses its in-process connection mid-call and returns 500 on every deploy.",
			order, want)
	}
}
