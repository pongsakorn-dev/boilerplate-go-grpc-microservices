package interceptor

import (
	"context"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/example/gomicro/internal/platform/apperr"
	"github.com/example/gomicro/internal/platform/auth"
)

// Auth verifies the caller's credential and enforces the authorisation policy.
//
// The two halves are deliberately one interceptor. Splitting authentication from
// authorisation into separate chain entries reads tidier and creates a window in which a
// request is authenticated but not yet authorised -- and any interceptor added between them
// runs for callers who have not been authorised for anything. Keeping them adjacent and
// atomic removes the question.
//
// Errors are apperr values, not gRPC statuses, because ErrorMap sits ABOVE this interceptor
// in the chain and maps them. Returning a status directly here would work today and would
// bypass the ErrorInfo/Reason details every other error path carries.
func Auth(v auth.Verifier, policy auth.Policy, log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		authCtx, err := authorize(ctx, v, policy, info.FullMethod, log)
		if err != nil {
			return nil, err
		}
		return handler(authCtx, req)
	}
}

// AuthStream is the streaming equivalent.
//
// It must wrap the ServerStream to override Context(): grpc.ServerStream returns the
// stream's own context and offers no setter, so without the wrapper a streaming handler
// calling auth.TenantFrom(ctx) finds nothing. That failure is invisible to every unary test,
// which is why grpcapi/auth_test.go::TestStreamingHandlersSeeThePrincipal reads the tenant
// off a streamed message rather than trusting this comment.
func AuthStream(v auth.Verifier, policy auth.Policy, log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		authCtx, err := authorize(ss.Context(), v, policy, info.FullMethod, log)
		if err != nil {
			return err
		}
		return handler(srv, WrapServerStream(ss, authCtx))
	}
}

// authorize is the whole decision, shared by both chains so they cannot drift.
func authorize(ctx context.Context, v auth.Verifier, policy auth.Policy, method string, log *slog.Logger) (context.Context, error) {
	rule, known := policy[method]

	// Unreachable in a server from grpcapi.NewServer, which refuses to start when the
	// policy misses a registered method. Deny anyway: "unreachable" describes today's
	// wiring, and the safe behaviour when that changes is refusal.
	if !known {
		log.ErrorContext(ctx, "no authorisation policy for method -- denying",
			slog.String("method", method),
			slog.String("fix", "add a Rule in internal/grpcapi/policy.go"))
		return nil, apperr.New(apperr.KindPermissionDenied, "NO_POLICY", "not authorised")
	}

	// Public methods skip verification ENTIRELY, and this is a throughput decision as much
	// as a correctness one. grpc.health.v1.Health/Check is public and Kubernetes calls it
	// from three probes on every pod every second; running signature verification on each
	// would spend real CPU proving nothing, on the one path that must stay cheap when the
	// service is already struggling.
	if rule.Public {
		return ctx, nil
	}

	// The raw credential is passed to the verifier even when absent, rather than being
	// rejected here for being empty. Deciding what counts as a credential belongs to the
	// verifier: OIDCVerifier rejects the empty string, and DevVerifier accepts it, which is
	// what keeps `grpcurl -plaintext localhost:50051 list` working on a fresh clone with no
	// identity provider. A short-circuit here would silently break that quickstart.
	principal, err := v.Verify(ctx, bearerToken(ctx))
	if err != nil {
		// Logged at WARN with the reason; the client gets only the code.
		//
		// Under credential stuffing this is one line per attempt, which is a real load on a
		// log pipeline. It is still the right default: authentication failures are the
		// highest-value security signal a service emits, and the flood-safe counterpart
		// already exists -- grpcprom's grpc_server_handled_total{grpc_code="Unauthenticated"}
		// is the series to alert on. A service under sustained attack should sample here.
		log.WarnContext(ctx, "authentication failed",
			slog.String("method", method), slog.String("reason", err.Error()))
		return nil, apperr.New(apperr.KindUnauthenticated, "UNAUTHENTICATED", "invalid or missing credentials")
	}

	if err := policy.Check(method, principal, true); err != nil {
		// The reason names the missing scope and stays in the log. Telling the client which
		// scope it lacks hands an attacker a shopping list for the next phishing attempt.
		log.WarnContext(ctx, "authorisation denied",
			slog.String("method", method),
			slog.String("subject", principal.Subject),
			slog.String("tenant", principal.TenantID),
			slog.String("reason", err.Error()))
		return nil, apperr.New(apperr.KindPermissionDenied, "PERMISSION_DENIED", "not authorised")
	}

	return auth.WithPrincipal(ctx, principal), nil
}

// bearerToken extracts the credential from the authorization metadata header.
//
// Returns "" when absent or malformed. The verifier decides what that means -- see the note
// at the call site.
func bearerToken(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	// gRPC lowercases metadata keys on the wire, so "authorization" is the only spelling
	// that can arrive. metadata.MD's Get does the canonicalisation anyway.
	values := md.Get("authorization")
	if len(values) == 0 {
		return ""
	}

	// RFC 6750 §2.1 makes the scheme case-insensitive. Clients send "Bearer", "bearer" and
	// occasionally "BEARER"; rejecting the last two produces a support ticket, not security.
	const prefix = "bearer "
	raw := values[0]
	if len(raw) < len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(raw[len(prefix):])
}
