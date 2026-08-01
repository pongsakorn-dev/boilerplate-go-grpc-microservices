package interceptor

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// Deadline arithmetic is tested under synctest so the assertions are EXACT equalities
// rather than "roughly 15 seconds, give or take scheduling".
//
// With a real clock the only honest assertion is a tolerance band, which means the test
// either flakes on a loaded CI runner or is too loose to catch an off-by-one. The bubble's
// clock does not advance unless every goroutine is blocked, so time.Until(deadline) is
// exactly what the code computed.
//
// This file deliberately imports no network package -- test/tiers_test.go enforces that,
// because a synctest bubble containing a real listener hangs forever.

func TestApplyDeadline(t *testing.T) {
	t.Parallel()

	const (
		defaultTimeout = 15 * time.Second
		maxTimeout     = 60 * time.Second
	)

	t.Run("no inbound deadline gets exactly the default", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			// A client that sets no deadline would otherwise run forever, holding a
			// goroutine, a database connection and an admission slot until restart.
			ctx, cancel := applyDeadline(context.Background(), defaultTimeout, maxTimeout)
			defer cancel()

			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("no deadline was applied; the RPC could run forever")
			}
			if got := time.Until(deadline); got != defaultTimeout {
				t.Errorf("timeout = %v, want exactly %v", got, defaultTimeout)
			}
		})
	})

	t.Run("an absurd inbound deadline is clamped to the maximum", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			// One client's mistake -- or a bug that passes an hour -- must not be able to
			// pin server resources for that long.
			parent, cancelParent := context.WithTimeout(context.Background(), time.Hour)
			defer cancelParent()

			ctx, cancel := applyDeadline(parent, defaultTimeout, maxTimeout)
			defer cancel()

			deadline, _ := ctx.Deadline()
			if got := time.Until(deadline); got != maxTimeout {
				t.Errorf("timeout = %v, want it clamped to %v", got, maxTimeout)
			}
		})
	})

	t.Run("a reasonable inbound deadline passes through untouched", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			// The caller knows better than this interceptor how long it is willing to
			// wait. Shortening a valid deadline would break a caller that legitimately
			// needs longer than the default.
			const clientTimeout = 10 * time.Millisecond

			parent, cancelParent := context.WithTimeout(context.Background(), clientTimeout)
			defer cancelParent()

			ctx, cancel := applyDeadline(parent, defaultTimeout, maxTimeout)
			defer cancel()

			deadline, _ := ctx.Deadline()
			if got := time.Until(deadline); got != clientTimeout {
				t.Errorf("timeout = %v, want the client's %v unchanged", got, clientTimeout)
			}
		})
	})

	t.Run("a deadline exactly at the maximum is not clamped", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			parent, cancelParent := context.WithTimeout(context.Background(), maxTimeout)
			defer cancelParent()

			ctx, cancel := applyDeadline(parent, defaultTimeout, maxTimeout)
			defer cancel()

			deadline, _ := ctx.Deadline()
			if got := time.Until(deadline); got != maxTimeout {
				t.Errorf("timeout = %v, want %v (boundary must not be clamped)", got, maxTimeout)
			}
		})
	})
}

// TestDeadlinePropagatesCancellation proves the derived context still dies with its parent.
//
// A WithTimeout that accidentally used context.Background() as its parent would produce a
// context that outlives the request -- so a cancelled client would leave the handler and
// every downstream call still running.
func TestDeadlinePropagatesCancellation(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		parent, cancelParent := context.WithCancel(context.Background())

		ctx, cancel := applyDeadline(parent, time.Minute, time.Hour)
		defer cancel()

		cancelParent()
		synctest.Wait()

		if ctx.Err() == nil {
			t.Error("cancelling the client's context did not cancel the handler's")
		}
	})
}
