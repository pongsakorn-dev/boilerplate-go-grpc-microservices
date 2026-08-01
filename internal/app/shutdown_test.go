package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
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
