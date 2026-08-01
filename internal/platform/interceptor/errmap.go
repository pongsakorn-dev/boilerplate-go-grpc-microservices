package interceptor

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/example/gomicro/internal/platform/apperr"
)

// ErrorMap converts handler errors into gRPC statuses with machine-readable details.
//
// Its POSITION in the chain is load-bearing and non-obvious. internal/grpcapi/chain.go is
// the single canonical explanation; it is deliberately not repeated here.
//
// (It used to be. This paragraph said "It is the INNERMOST interceptor" while chain.go said
// in capitals that innermost is wrong -- the one argument written at four sites is the one
// that drifted into a flat contradiction, at six months old with a single author. That is
// the empirical case for one canonical home per argument.)
//
// Short version: below logging and metrics, above everything that produces an error.
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
