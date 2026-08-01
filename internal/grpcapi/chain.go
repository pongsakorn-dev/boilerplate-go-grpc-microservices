package grpcapi

import (
	"fmt"
	"log/slog"

	"buf.build/go/protovalidate"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/stats"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
	"github.com/example/gomicro/internal/order"
	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/platform/interceptor"
	"github.com/example/gomicro/internal/platform/observability"
)

// Deps is everything NewServer needs. A struct rather than a long parameter list so adding
// a dependency does not silently reorder two same-typed arguments at a call site.
type Deps struct {
	Log          *slog.Logger
	Cfg          config.Config
	OrderService *order.Service
	Health       *health.Server
	Metrics      *observability.Metrics
	Validator    protovalidate.Validator
}

// NewServer builds the production gRPC server: the real interceptor chain, the real
// hardening options, health, and reflection.
//
// Tests call this exact function through testutil.NewTestServer rather than assembling
// their own server. That is what stops the test harness drifting into a parallel wiring
// that passes while production fails.
func NewServer(d Deps) (*grpc.Server, error) {
	// Auth is SELECTED from configuration, never hard-wired.
	//
	// This function used to install DevAuth unconditionally and never read Cfg.AuthMode at
	// all, which meant AUTH_MODE=oidc was silently ignored and the service ran with no
	// authentication while appearing configured. config.Validate now refuses that value,
	// but the durable fix is structural: the switch exists BEFORE the second verifier does,
	// with a default arm that returns an error rather than falling through to a permissive
	// one. A factory that cannot express "I do not know this mode" is how fail-open
	// happens.
	authUnary, authStream, err := authInterceptors(d.Cfg)
	if err != nil {
		return nil, err
	}

	// THE CHAIN. grpc-go applies these outermost-first, so the LAST entry is closest to
	// the handler. chain_test.go proves the ordering behaviourally -- with no test hooks
	// inside the production interceptors -- because an ordering documented only in a
	// comment is an ordering that drifts.
	//
	//  recovery   outermost, so a panic ANYWHERE inside is contained
	//  metrics    outside admission, so shed load is still counted
	//  logging    observes the FINAL code, because errmap sits below it
	//  errmap     see the note below -- its position is the subtle one
	//  admission  BEFORE auth: shed a flood before paying for signature verification
	//  auth       establishes the principal (M5 swaps in real verification)
	//  deadline   bounds the handler and everything it calls downstream
	//  validate   after auth, so an anonymous caller cannot probe the schema
	//
	// ERRMAP IS NOT INNERMOST, and that is a correction worth spelling out.
	//
	// "Innermost" is the intuitive placement and it is wrong: an interceptor only sees
	// errors from what it WRAPS. Placed last, errmap wraps the handler alone -- so a
	// rejection from admission, auth or validate would sail past it unmapped and reach the
	// client as codes.Unknown. Those are the errors clients hit most often.
	//
	// The real requirement is two-sided: errmap must sit BELOW logging and metrics (so they
	// record the mapped code rather than Unknown) and ABOVE every interceptor that produces
	// an error (so those errors get mapped at all). This position satisfies both.
	unary := []grpc.UnaryServerInterceptor{
		interceptor.Recovery(d.Log),
		d.Metrics.Server.UnaryServerInterceptor(),
		interceptor.Logging(d.Log),
		interceptor.ErrorMap(d.Cfg.ServiceName),
		interceptor.Admission(d.Cfg.Server.AdmissionLimit),
		authUnary,
		interceptor.Deadline(d.Cfg.Server.DefaultTimeout, d.Cfg.Server.MaxTimeout),
		interceptor.Validate(d.Validator),
	}

	// The stream chain is SHORTER than the unary one, and that is a known gap rather than
	// an oversight: it has no Admission and no Validate.
	//
	// Validate is genuinely not applicable -- a unary interceptor sees one request message,
	// while a stream's messages arrive inside the handler, so per-message validation has to
	// live there.
	//
	// Admission is a real open question. Holding a concurrency slot for the entire life of
	// a long-lived watch would let ten idle subscribers exhaust a limiter sized for the
	// database pool, which is worse than not limiting them. Today streams are bounded only
	// by grpc.MaxConcurrentStreams (250). A fork that adds streaming work touching the
	// database MUST revisit this -- see docs/adr/ when it lands.
	stream := []grpc.StreamServerInterceptor{
		interceptor.RecoveryStream(d.Log),
		d.Metrics.Server.StreamServerInterceptor(),
		interceptor.LoggingStream(d.Log),
		interceptor.ErrorMapStream(d.Cfg.ServiceName),
		authStream,
		interceptor.DeadlineStream(d.Cfg.Server.MaxTimeout),
	}

	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(stream...),

		// otelgrpc as a StatsHandler, NOT as an interceptor.
		//
		// The interceptor form is deprecated and cannot see everything: a StatsHandler
		// observes the connection and per-message lifecycle, so streaming RPCs and
		// message-level events are instrumented correctly rather than appearing as one
		// opaque span.
		grpc.StatsHandler(otelgrpc.NewServerHandler(
			// Health checks produce no spans. Kubernetes probes this once a second from
			// three probes on every pod; at three replicas that is ~780k spans a day that
			// say only "still alive", burying real traces and costing real money.
			otelgrpc.WithFilter(func(info *stats.RPCTagInfo) bool {
				return observability.FilterHealthChecks(info.FullMethodName)
			}),
		)),

		// grpc-go's default MaxConcurrentStreams is effectively unbounded, so a single
		// client can open enough concurrent streams to exhaust memory. A DoS control, not
		// a tuning knob.
		grpc.MaxConcurrentStreams(d.Cfg.Server.MaxConcurrentStreams),

		// Bound the largest message the server will decode; without it a 2 GB body is
		// allocated before any handler runs.
		grpc.MaxRecvMsgSize(d.Cfg.Server.MaxRecvMsgBytes),

		// MaxConnectionAge forces a periodic GOAWAY so clients reconnect.
		//
		// Without it, HTTP/2 connections stay pinned to whichever pods existed when the
		// client started, and a rolling deploy never rebalances: new pods sit idle while
		// old connections keep hammering whatever survived. The grace period lets in-flight
		// RPCs finish before the connection actually closes.
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:      d.Cfg.Server.MaxConnectionAge,
			MaxConnectionAgeGrace: d.Cfg.Server.MaxConnectionAgeGrace,
		}),

		// Reject clients pinging more aggressively than this. An unbounded ping rate is a
		// cheap way to burn server CPU.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             d.Cfg.Server.KeepaliveMinTime,
			PermitWithoutStream: false,
		}),
	}

	srv := grpc.NewServer(opts...)

	orderv1.RegisterOrderServiceServer(srv, NewOrderServer(d.OrderService))

	// Health lives on the RPC listener, not the admin listener.
	//
	// Kubernetes' native grpc: probe (GA since 1.27) dials the RPC port and calls
	// grpc.health.v1.Health/Check. Registering health anywhere else means the probe cannot
	// reach it -- and it also stops the probe from testing the thing that actually serves
	// traffic. M4 will add a test that dials a real port to prove the probe path works.
	healthpb.RegisterHealthServer(srv, d.Health)

	// Reflection is what makes `grpcurl -plaintext localhost:50051 list` work with no
	// .proto on hand. Enabled unconditionally because this is a template and
	// discoverability matters more than the schema it exposes; a service handling
	// sensitive schemas should gate it on APP_ENV.
	reflection.Register(srv)

	// Initialize metric series for every registered method.
	//
	// Without this, a method that has never been called has no time series at all -- so a
	// PromQL alert on its error rate silently evaluates to "no data" rather than zero, and
	// the alert never fires because the thing it watches does not exist yet.
	d.Metrics.Server.InitializeMetrics(srv)

	return srv, nil
}

// authInterceptors picks the verifier for the configured AUTH_MODE.
//
// The default arm returns an error and never a permissive fallback. That is the whole
// point: an unrecognised mode must stop the process, because the alternative -- quietly
// using dev auth -- is a service that looks configured and authenticates nobody.
func authInterceptors(cfg config.Config) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor, error) {
	switch cfg.AuthMode {
	case config.AuthDev:
		return interceptor.DevAuth(), interceptor.DevAuthStream(), nil

	case config.AuthOIDC:
		// Unreachable today: config.Validate refuses this value before New is ever called.
		// The arm exists so that M5 is a change to THIS switch rather than a change to the
		// shape of the function -- and so that removing the Validate guard without
		// implementing the verifier fails loudly instead of falling back to dev.
		return nil, nil, fmt.Errorf(
			"AUTH_MODE=%s: the OIDC verifier is not implemented yet (milestone M5)", cfg.AuthMode)

	default:
		return nil, nil, fmt.Errorf("unknown AUTH_MODE %q", cfg.AuthMode)
	}
}
