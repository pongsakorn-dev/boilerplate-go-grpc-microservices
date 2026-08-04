package interceptor

import (
	"context"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/example/gomicro/internal/platform/apperr"
	"github.com/example/gomicro/internal/platform/observability"
)

// Admission bounds the number of RPCs executing concurrently.
//
// This is a CONCURRENCY limiter, not a rate limiter, and the distinction is the whole
// point. A per-replica rate limit configured at "100 requests per second" silently becomes
// 100 x replicas in production, resets to zero on every deploy, and changes meaning every
// time the HPA scales -- it is a number that looks like a control and is not one.
//
// Concurrency is different: it maps directly onto the resource that actually runs out.
// The limit is sized from the database pool, because that is the real bottleneck. Admitting
// more work than you have connections to execute does not increase throughput; it converts
// a fast, honest rejection into a slow timeout, while the queued requests hold memory and
// their callers' deadlines burn down.
//
// Placed BEFORE auth in the chain on purpose: shedding a flood must not require verifying
// a JWT first, or the flood costs you signature verification on every request -- which is
// exactly the CPU an attacker wants to consume.
//
// Distributed, per-tenant quotas are a different job and live in the Redis rate limiter
// (M7), which runs AFTER auth because it needs a verified tenant.
//
// THE HEALTH CHECK IS EXEMPT, and leaving it un-exempt made this limiter dangerous in
// exactly the situation it exists for. See the note on the exemption below.
func Admission(limit int) grpc.UnaryServerInterceptor {
	// A buffered channel is the whole implementation. Non-blocking acquire is a select
	// with a default, which is precisely the "reject immediately, never queue" semantics
	// we want -- no dependency required.
	slots := make(chan struct{}, max(limit, 1))

	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		// The liveness probe must be answerable while the service is shedding.
		//
		// grpcapi registers the health server on the SAME grpc.Server as the business
		// methods, because Kubernetes' native grpc: probe dials the port that actually
		// serves traffic -- which is the right decision, and it means health checks run
		// through this chain like anything else.
		//
		// So under saturation the limiter answered the kubelet with ResourceExhausted, the
		// liveness probe failed, and the pod was RESTARTED. That removes a replica from an
		// already-overloaded service, moves its share of the load onto the remaining pods,
		// and saturates the next one: a load shedder that kills the thing proving you are
		// alive turns a survivable overload into a rolling outage.
		//
		// A health check takes no database connection, so exempting it costs nothing that
		// the limit is protecting. The constant is observability's, deliberately: the trace
		// filter suppresses exactly this method, and two copies of the string would let the
		// two behaviours drift apart.
		//
		// Health/Watch needs no equivalent -- it is a streaming method, and the stream chain
		// has no Admission at all (see the note in grpcapi/chain.go).
		if info != nil && info.FullMethod == observability.HealthCheckMethod {
			return handler(ctx, req)
		}

		// A request whose deadline has ALREADY expired is rejected without taking a slot.
		//
		// This matters under load: when a queue builds, the requests reaching the front
		// are the oldest, and their callers have usually given up. Executing them burns a
		// slot and a database connection to produce a response nobody will read, which
		// deepens the very queue causing the problem.
		if deadline, ok := ctx.Deadline(); ok && time.Now().After(deadline) {
			return nil, apperr.New(apperr.KindResourceExhausted, "DEADLINE_ALREADY_EXPIRED",
				"request deadline expired before admission")
		}

		select {
		case slots <- struct{}{}:
			// Release on EVERY exit path, including a panic. Without the defer, a handler
			// that panics permanently consumes a slot, and enough panics wedge the service
			// into rejecting everything -- with no error that points at this file.
			defer func() { <-slots }()
			return handler(ctx, req)

		default:
			return nil, overloaded()
		}
	}
}

// overloaded builds a ResourceExhausted carrying RetryInfo.
//
// RetryInfo is not decoration: gRPC clients and service meshes read it to schedule a
// retry. Without it, a well-behaved client retries immediately, which is the worst possible
// response to an overloaded server -- the retries themselves become the load.
func overloaded() error {
	return apperr.New(apperr.KindResourceExhausted, "SERVER_OVERLOADED",
		"too many concurrent requests").
		WithDetails(&errdetails.RetryInfo{
			RetryDelay: durationpb.New(100 * time.Millisecond),
		})
}
