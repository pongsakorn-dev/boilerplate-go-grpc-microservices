package client

import (
	"context"
	"time"

	"google.golang.org/grpc"
)

// budget decides how long an outbound call may take, given how long the inbound one has left.
//
// THE POINT IS THE HEADROOM, and it is the one thing a service mesh cannot do for you. A mesh
// can enforce a timeout on the call; it cannot arrange for YOUR handler to still be running
// when that timeout fires. If an outbound call is given every millisecond the caller allowed,
// then when the upstream is slow the caller's deadline and yours expire together: your handler
// is cancelled mid-flight, you write no log line worth having, you record no metric, and your
// caller sees a bare DeadlineExceeded that says nothing about which of your dependencies was
// responsible. Reserving a slice means the upstream call fails FIRST, inside your handler,
// where you can name the upstream, count it, and decide what to return.
//
// Three cases, in order:
//
//	no deadline at all   ->  use defaultTimeout. An outbound RPC with no deadline is a
//	                         goroutine and a connection held until the upstream feels like
//	                         answering, which under load is the definition of never.
//	enough time left     ->  remaining minus the reserved fraction.
//	not enough left      ->  fail immediately, WITHOUT dialling. See errBudgetExhausted.
//
// It is a pure function of (now-relative deadline, settings) so it can be tested exhaustively
// in a synctest bubble with no network -- the same split the server's applyDeadline uses.
func budget(ctx context.Context, defaultTimeout, minBudget time.Duration, reserve float64) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return defaultTimeout, true
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, false
	}

	// The reserved slice is a FRACTION rather than a fixed duration because the useful
	// headroom scales with the call: 50ms of slack is generous inside a 200ms budget and
	// invisible inside a 30-second one.
	allowed := time.Duration(float64(remaining) * (1 - reserve))

	// Below minBudget the call is not worth making. Dialling anyway spends a connection, a
	// goroutine and an upstream handler on work that is already known to be too late -- and
	// the upstream cannot tell that its answer will be discarded, so it does the whole job.
	if allowed < minBudget {
		return 0, false
	}
	return allowed, true
}

// WithBudget returns the unary interceptor that applies the deadline budget.
//
// It is separate from Dial's own wiring so a caller assembling a connection by hand gets the
// same behaviour, and so the interceptor can be tested without a server.
func (c *Options) unaryBudget() grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		allowed, ok := budget(ctx, c.DefaultTimeout, c.MinBudget, c.ReserveFraction)
		if !ok {
			return newBudgetError(c.Target, method, ctx)
		}

		// WithTimeout, not WithDeadline on a fresh context: this must keep the caller's
		// cancellation, its trace span and its metadata. Detaching here would leave the
		// upstream call running after the caller had already given up.
		ctx, cancel := context.WithTimeout(ctx, allowed)
		defer cancel()

		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// streamBudget is the streaming twin.
//
// A STREAM'S BUDGET IS NOT ITS DEADLINE, and conflating them is how a long-lived subscription
// gets cut off after nine seconds. The context here bounds the whole stream, so the budget is
// applied only to bound how long ESTABLISHING it may take -- after which the deadline that
// matters is whatever the caller set on the stream context.
//
// The cancel deliberately outlives this function, tied to the returned stream instead: a
// deferred cancel would tear the stream down the moment NewStream returned.
func (c *Options) streamBudget() grpc.StreamClientInterceptor {
	return func(
		ctx context.Context,
		desc *grpc.StreamDesc,
		cc *grpc.ClientConn,
		method string,
		streamer grpc.Streamer,
		opts ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		if _, ok := budget(ctx, c.DefaultTimeout, c.MinBudget, c.ReserveFraction); !ok {
			return nil, newBudgetError(c.Target, method, ctx)
		}
		return streamer(ctx, desc, cc, method, opts...)
	}
}
