package interceptor

import (
	"context"

	"google.golang.org/grpc"

	"github.com/example/gomicro/internal/platform/auth"
)

// DevPrincipal is the identity injected when AUTH_MODE=dev.
//
// It exists for exactly one reason: so that `git clone && go run ./cmd/orderd` works with
// no identity provider, no keys, and no configuration. That is the difference between a
// template someone tries and a template someone closes.
//
// It is guarded in three places, because an accidental production deploy with dev auth is
// a total authentication bypass:
//
//   - config.Validate refuses AUTH_MODE=dev when APP_ENV=production, before any listener
//     opens (config_test.go::TestValidate_RejectsDevAuthOutsideDev).
//   - app.New logs a WARN on every single startup, not once.
//   - the deploy overlay sets AUTH_MODE=oidc explicitly rather than relying on a default.
var DevPrincipal = auth.Principal{
	Subject:  "dev-user",
	TenantID: "dev-tenant",
	Scopes:   []string{"orders:read", "orders:write"},
}

// DevAuth injects a fixed principal without verifying anything.
//
// M5 replaces this with a real verifier chain (dev static | OIDC JWKS) plus a
// default-deny policy map. The interface it populates -- auth.Principal in the context --
// is already the real one, so nothing downstream changes when the verifier does.
func DevAuth() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return handler(auth.WithPrincipal(ctx, DevPrincipal), req)
	}
}

// DevAuthStream is the streaming equivalent.
//
// It has to wrap the ServerStream to override Context(), because grpc.ServerStream
// returns the stream's own context and there is no setter. Without the wrapper the
// principal is invisible inside a streaming handler -- a bug no unary test can catch,
// which is why stream_test.go asserts it directly.
func DevAuthStream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return handler(srv, &wrappedStream{
			ServerStream: ss,
			ctx:          auth.WithPrincipal(ss.Context(), DevPrincipal),
		})
	}
}

// wrappedStream overrides Context so interceptor-injected values reach stream handlers.
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }
