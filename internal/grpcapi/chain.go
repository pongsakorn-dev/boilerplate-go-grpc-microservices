package grpcapi

import (
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
	"github.com/example/gomicro/internal/order"
	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/platform/interceptor"
)

// Deps is everything NewServer needs. A struct rather than a long parameter list so
// adding a dependency does not silently reorder two same-typed arguments at a call site.
type Deps struct {
	Log          *slog.Logger
	Cfg          config.Config
	OrderService *order.Service
	Health       *health.Server
}

// NewServer builds the production gRPC server: the real interceptor chain, the real
// hardening options, health, and reflection.
//
// Tests call this exact function through testutil.NewTestServer rather than assembling
// their own server. That is what stops the test harness drifting into a parallel wiring
// that passes while production fails -- server_test.go additionally asserts the
// registered service set matches.
func NewServer(d Deps) *grpc.Server {
	// Chain order. grpc-go applies these outermost-first, so the LAST entry is closest
	// to the handler. chain_test.go proves the ordering behaviourally.
	//
	//   recovery  -- outermost, so a panic anywhere inside is contained
	//   devauth   -- establishes the principal (M5 replaces with real verification)
	//   errmap    -- INNERMOST, so everything outside sees the final status code
	unary := []grpc.UnaryServerInterceptor{
		interceptor.Recovery(d.Log),
		interceptor.DevAuth(),
		interceptor.ErrorMap(d.Cfg.ServiceName),
	}
	stream := []grpc.StreamServerInterceptor{
		interceptor.RecoveryStream(d.Log),
		interceptor.DevAuthStream(),
		interceptor.ErrorMapStream(d.Cfg.ServiceName),
	}

	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(stream...),

		// grpc-go's default MaxConcurrentStreams is effectively unbounded, so a single
		// client can open enough concurrent streams to exhaust memory. This is a DoS
		// control, not a tuning knob.
		grpc.MaxConcurrentStreams(d.Cfg.Server.MaxConcurrentStreams),

		// Bound the largest message the server will decode. Without it, a 2 GB body is
		// allocated before any handler runs.
		grpc.MaxRecvMsgSize(d.Cfg.Server.MaxRecvMsgBytes),

		// MaxConnectionAge forces a periodic GOAWAY so clients reconnect.
		//
		// Without it, HTTP/2 connections are pinned to whichever pods existed when the
		// client started. A rolling deploy then never rebalances: the new pods sit idle
		// while the old connections keep hammering whatever survived. The grace period
		// lets in-flight RPCs finish before the connection actually closes.
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:      d.Cfg.Server.MaxConnectionAge,
			MaxConnectionAgeGrace: d.Cfg.Server.MaxConnectionAgeGrace,
		}),

		// Reject clients that ping more aggressively than this. An unbounded ping rate is
		// a cheap way to burn server CPU.
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
	// grpc.health.v1.Health/Check. Registering health anywhere else means the probe
	// cannot reach it -- and it also means the probe stops testing the thing that
	// actually serves traffic. probe_test.go dials a real port to prove this works.
	healthpb.RegisterHealthServer(srv, d.Health)

	// Reflection is what makes `grpcurl -plaintext localhost:50051 list` work with no
	// .proto file on hand. It is enabled unconditionally here because this is a template
	// and discoverability matters more than the small amount of schema it exposes; a
	// service handling sensitive schemas should gate it on APP_ENV.
	reflection.Register(srv)

	return srv
}
