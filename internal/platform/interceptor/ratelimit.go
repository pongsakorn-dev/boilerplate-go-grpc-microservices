package interceptor

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/example/gomicro/internal/platform/apperr"
	"github.com/example/gomicro/internal/platform/auth"
	"github.com/example/gomicro/internal/platform/ratelimit"
)

// RateLimit enforces the distributed per-tenant quota.
//
// PLACED AFTER AUTH, and it has to be: the key is the tenant, and the tenant comes from the
// verified token. Before auth there is no identity to bill, so the only available key would
// be something client-controlled -- which is not a quota, it is a suggestion.
//
// The cost is that unauthenticated floods are not limited here. That is deliberate and
// covered elsewhere: Admission runs BEFORE auth precisely so a flood is shed without paying
// for signature verification. Two mechanisms, two positions, two jobs.
func RateLimit(limiter ratelimit.Limiter, log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		principal, ok := auth.PrincipalFrom(ctx)
		if !ok || principal.TenantID == "" {
			// No principal means a Public method -- health, which the kubelet calls once a
			// second from three probes on every pod. Rate limiting the liveness probe is how
			// a service throttles itself out of its own Service endpoints.
			return handler(ctx, req)
		}

		key := limitKey(principal.TenantID, info.FullMethod)

		result, err := limiter.Allow(ctx, key)
		if err != nil {
			// FAIL OPEN, and this is the opposite of what auth does two lines up the chain.
			//
			// The difference is what each protects. Auth protects DATA: without it a request
			// may read something it must not, so an auth failure must deny. The limiter
			// protects CAPACITY: without it requests are merely unthrottled. Failing closed
			// here would mean an unreachable Redis rejects every request on every replica --
			// converting "quotas are temporarily unenforced" into a total outage, and making
			// the limiter a hard dependency of serving at all.
			//
			// It must be LOUD, though, or "the limiter has been off for a month" is a thing
			// you discover from a bill. grpcprom already counts outcomes; this log names the
			// cause.
			log.WarnContext(ctx, "rate limiter unavailable, allowing the request",
				slog.String("method", info.FullMethod),
				slog.String("tenant", principal.TenantID),
				slog.String("error", err.Error()))
			return handler(ctx, req)
		}

		if !result.Allowed {
			// Retry-After reaches BOTH surfaces.
			//
			// RetryInfo is the gRPC detail that clients and service meshes read to schedule a
			// retry. The header is what survives transcoding to REST -- gateway/errors.go
			// forwards `retry-after` specifically. Without them a well-behaved client retries
			// immediately, and the retries become the load.
			setRetryHeaders(ctx, result)

			return nil, apperr.New(apperr.KindResourceExhausted, "RATE_LIMITED",
				"request quota exceeded").
				WithMetadata(map[string]string{"retry_after": result.RetryAfter.String()}).
				WithDetails(&errdetails.RetryInfo{
					RetryDelay: durationpb.New(result.RetryAfter),
				})
		}

		setRemainingHeader(ctx, result)
		return handler(ctx, req)
	}
}

// limitKey scopes the quota to one tenant and one method.
//
// TENANT+METHOD RATHER THAN TENANT ALONE, which is a real trade-off rather than an obvious
// choice. Per-method means a tenant calling five methods gets five times the configured
// limit in aggregate. Per-tenant-only means one hot method starves every other call that
// tenant makes, so a burst of list requests takes down its checkout flow.
//
// Per-method is chosen because the failure it permits (more total traffic from a paying
// tenant) is milder than the one it prevents (one endpoint denying a tenant its whole API).
// A fork that sells a single aggregate quota changes this one function -- and should also
// then set the limit accordingly.
func limitKey(tenantID, fullMethod string) string {
	return "ratelimit:" + tenantID + ":" + fullMethod
}

// setRetryHeaders puts the backoff advice on the response metadata.
func setRetryHeaders(ctx context.Context, result ratelimit.Result) {
	// Seconds, rounded UP: RFC 9110's Retry-After is integer seconds, and rounding down
	// would advise a client to come back before it is actually welcome.
	seconds := int64(result.RetryAfter.Seconds())
	if result.RetryAfter > time.Duration(seconds)*time.Second {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}

	_ = grpc.SetHeader(ctx, metadata.Pairs(
		"retry-after", strconv.FormatInt(seconds, 10),
	))
}

// setRemainingHeader reports leftover burst capacity, so a well-behaved client can slow down
// BEFORE it is rejected rather than discovering the limit by hitting it.
func setRemainingHeader(ctx context.Context, result ratelimit.Result) {
	if result.Remaining < 0 {
		return // AllowAll reports -1: there is no limit to have capacity in.
	}
	_ = grpc.SetHeader(ctx, metadata.Pairs(
		"x-ratelimit-remaining", strconv.Itoa(result.Remaining),
	))
}
