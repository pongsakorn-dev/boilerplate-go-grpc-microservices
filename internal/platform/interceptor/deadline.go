package interceptor

import (
	"context"
	"time"

	"google.golang.org/grpc"
)

// Deadline guarantees every RPC has a bounded lifetime.
//
// Two failures it prevents, both of which look like a resource leak rather than a missing
// timeout:
//
//   - A client that sets NO deadline. gRPC is happy to let that RPC run forever, holding a
//     goroutine, a database connection, and an admission slot until the process restarts.
//     A handful of these is an outage with no error in the logs.
//   - A client that sets an ABSURD deadline (an hour, or a bug that passes a zero value
//     meaning "no timeout"). One client's mistake should not be able to pin your resources.
//
// A deadline already in range is passed through untouched: the caller knows better than
// this interceptor how long it is willing to wait, and shortening it would break a caller
// that legitimately needs longer than the default.
//
// Deadlines PROPAGATE automatically over gRPC, so clamping here also bounds every
// downstream call this request makes.
func Deadline(defaultTimeout, maxTimeout time.Duration) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, cancel := applyDeadline(ctx, defaultTimeout, maxTimeout)
		defer cancel()
		return handler(ctx, req)
	}
}

// DeadlineStream is the streaming equivalent.
//
// The default/clamp values are usually far too short for a long-lived stream, so this
// applies only the MAXIMUM, and only when the client set no deadline at all.
func DeadlineStream(maxTimeout time.Duration) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := ss.Context()
		if _, ok := ctx.Deadline(); ok {
			return handler(srv, ss)
		}
		ctx, cancel := context.WithTimeout(ctx, maxTimeout)
		defer cancel()
		return handler(srv, WrapServerStream(ss, ctx))
	}
}

// applyDeadline is separated out so deadline_test.go can assert the arithmetic directly
// under testing/synctest, where the clock is deterministic and equality is exact.
func applyDeadline(ctx context.Context, defaultTimeout, maxTimeout time.Duration) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithTimeout(ctx, defaultTimeout)
	}

	if remaining := time.Until(deadline); remaining > maxTimeout {
		return context.WithTimeout(ctx, maxTimeout)
	}

	// In range: leave the caller's deadline exactly as it is.
	return context.WithCancel(ctx)
}
