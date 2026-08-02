package interceptor

import (
	"context"

	"google.golang.org/grpc"
)

// wrappedServerStream overrides Context so interceptor-injected values reach a streaming
// handler.
//
// grpc.ServerStream exposes Context() but has no setter, and the context it returns is
// fixed when the stream is created. So an interceptor that adds a value to the context --
// the authenticated principal, a trace span, a deadline -- has no way to make a streaming
// handler see it without replacing the stream object entirely.
//
// The consequence of getting this wrong is invisible in every unary test: unary
// interceptors take and return a context directly, so they work. Streams silently do not,
// and a streaming handler calling auth.TenantFrom(ctx) simply finds nothing.
// grpcapi/auth_test.go::TestStreamingHandlersSeeThePrincipal asserts the principal IS
// visible inside a streaming handler, by reading the tenant off a streamed message.
type wrappedServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedServerStream) Context() context.Context { return w.ctx }

// WrapServerStream returns a ServerStream whose Context reports ctx.
//
// If ctx is unchanged the original stream is returned, so the common case allocates
// nothing.
func WrapServerStream(ss grpc.ServerStream, ctx context.Context) grpc.ServerStream {
	if ss.Context() == ctx {
		return ss
	}
	return &wrappedServerStream{ServerStream: ss, ctx: ctx}
}
