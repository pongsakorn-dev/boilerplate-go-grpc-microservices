// Package interceptor holds the server-side gRPC middleware chain.
//
// Order matters and is asserted behaviourally in internal/grpcapi/chain_test.go rather
// than documented and hoped for.
package interceptor

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	"google.golang.org/grpc"

	"github.com/example/gomicro/internal/platform/apperr"
)

// Recovery converts a panicking handler into an Internal error instead of taking the
// whole process down.
//
// A panic in one RPC killing every in-flight request on the pod is a much worse outage
// than one failed request. The stack goes to the log; the client gets "internal error"
// and nothing else, because a stack trace names your packages, file paths, and often
// the arguments that caused the panic.
//
// There is ONE recovery interceptor, not two. The "outer guard in case logging panics"
// trick protects against a bug you should fix rather than absorb.
//
// IT TAKES A DOMAIN, AND MAPS ITS OWN ERROR. That is not symmetry with ErrorMap for its own
// sake -- it is a fix for a real leak.
//
// Recovery is the OUTERMOST interceptor, so nothing above it can map what it returns. It
// previously returned a bare *apperr.Error, and grpc-go, finding no gRPC status on it, fell
// back to status.New(codes.Unknown, err.Error()) -- which is the apperr's Error() string,
// which embeds the panic value. A handler panicking on a connection string sent the client:
//
//	code = Unknown
//	msg  = "PANIC: handler panicked: panic: postgres://user:hunter2@db.internal:5432/orders"
//
// The comment above promised the opposite, and had said so since M3. Every other interceptor
// can return a raw apperr because ErrorMap sits above them; Recovery is the one that cannot,
// and that exception is exactly what got missed. recovery_test.go now drives the real chain
// in production order rather than calling apperr.ToStatus by hand, which is what let this
// survive: the old test performed the mapping step that production skipped.
func Recovery(log *slog.Logger, domain string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = handlePanic(ctx, log, domain, info.FullMethod, r)
			}
		}()
		return handler(ctx, req)
	}
}

// RecoveryStream is the streaming equivalent. Streams need their own interceptor: a
// unary interceptor never runs for a streaming RPC, so a panic in a stream handler
// would go unguarded.
func RecoveryStream(log *slog.Logger, domain string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = handlePanic(ss.Context(), log, domain, info.FullMethod, r)
			}
		}()
		return handler(srv, ss)
	}
}

func handlePanic(ctx context.Context, log *slog.Logger, domain, method string, r any) error {
	stack := debug.Stack()

	// The panic value and the stack are logged, never returned.
	log.ErrorContext(ctx, "panic recovered in handler",
		slog.String("grpc.method", method),
		slog.Any("panic", r),
		slog.String("stack", string(stack)),
	)

	cause := fmt.Errorf("panic: %v", r)
	appErr := apperr.Wrap(cause, apperr.KindInternal, "PANIC", "handler panicked")

	// Mapped HERE, not left to ErrorMap. See the note on Recovery.
	return apperr.ToError(appErr, domain)
}
