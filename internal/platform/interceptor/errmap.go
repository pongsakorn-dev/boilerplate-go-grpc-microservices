package interceptor

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/example/gomicro/internal/platform/apperr"
)

// ErrorMap converts handler errors into gRPC statuses with machine-readable details.
//
// It is the INNERMOST interceptor, which means it runs last on the way in and first on
// the way out. That placement is load-bearing: every interceptor outside it -- logging,
// metrics, tracing -- then observes the FINAL status code. Put it anywhere else and your
// dashboards fill with codes.Unknown for errors you carefully classified, because
// logging saw the raw error before it was mapped.
//
// chain_test.go asserts this behaviourally: a domain not-found must appear as NotFound in
// both the log record and the grpc_server_handled_total metric, not as Unknown.
func ErrorMap(domain string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err == nil {
			return resp, nil
		}
		return resp, mapErr(err, domain)
	}
}

// ErrorMapStream is the streaming equivalent.
func ErrorMapStream(domain string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := handler(srv, ss); err != nil {
			return mapErr(err, domain)
		}
		return nil
	}
}

func mapErr(err error, domain string) error {
	// A handler that already produced a gRPC status (or a library below it did) is left
	// alone. Re-wrapping would turn a deliberate codes.Canceled from a cancelled stream
	// into codes.Internal and make every client disconnect look like a server bug.
	if _, ok := status.FromError(err); ok {
		if _, isApp := apperr.From(err); !isApp {
			return err
		}
	}
	return apperr.ToError(err, domain)
}
