package app

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Step is one unit of shutdown work.
type Step struct {
	Name    string
	Timeout time.Duration
	Fn      func(context.Context) error
}

// Shutdown runs steps in REVERSE order with a per-step timeout.
//
// This function is deliberately PURE: it performs no I/O of its own, binds no listener,
// and knows nothing about gRPC. That is what makes it testable under testing/synctest,
// where a 30-second configured drain resolves in microseconds and the ordering assertions
// are exact rather than timing-dependent.
//
// Running the REAL app.Run inside a synctest bubble would not work: grpc-go's transport
// goroutines block on operations the runtime does not consider durably blocking, so the
// bubble either reports a deadlock or hangs. A hanging test in the default tier is
// adoption-fatal, because a newcomer cannot tell whether they broke it. So the sequencing
// logic is tested here in a bubble, and the actual drain behaviour is tested against a
// real bufconn server, which M4 will add. Two tests, each testing what it can
// actually observe.
//
// Reverse order matters: dependencies are opened outermost-first (config, then database,
// then server), so they must close innermost-first or the server will still be accepting
// requests against a closed pool.
//
// Every step runs even if an earlier one fails. Abandoning the rest on the first error
// would leak whatever the remaining steps were meant to release.
func Shutdown(ctx context.Context, steps []Step) error {
	var errs []error

	for i := len(steps) - 1; i >= 0; i-- {
		step := steps[i]
		if step.Fn == nil {
			continue
		}

		timeout := step.Timeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}

		stepCtx, cancel := context.WithTimeout(ctx, timeout)
		err := step.Fn(stepCtx)
		cancel()

		if err != nil {
			errs = append(errs, fmt.Errorf("shutdown step %q: %w", step.Name, err))
		}
	}

	return errors.Join(errs...)
}
