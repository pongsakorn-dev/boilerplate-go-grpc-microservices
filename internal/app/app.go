// Package app is the composition root.
//
// Every dependency in the service is constructed here, in one readable function, in
// dependency order. There is no DI container and no code generation: a newcomer traces
// the entire object graph by reading New from top to bottom, and a missing dependency is
// a compile error rather than a runtime panic during startup.
//
// Adopting uber-go/fx or wire later is purely additive and touches zero business code.
// Unwinding either is a rewrite. So the template starts with the reversible option.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"buf.build/go/protovalidate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"

	"github.com/example/gomicro/internal/gateway"
	"github.com/example/gomicro/internal/grpcapi"
	"github.com/example/gomicro/internal/order"
	"github.com/example/gomicro/internal/order/ordermem"
	"github.com/example/gomicro/internal/order/orderpg"
	"github.com/example/gomicro/internal/platform/auth"
	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/platform/gormx"
	"github.com/example/gomicro/internal/platform/observability"
)

// App is a fully wired service, not yet listening.
type App struct {
	cfg config.Config
	log *slog.Logger

	grpcServer *grpc.Server
	health     *health.Server
	adminSrv   *http.Server
	metrics    *observability.Metrics

	// The REST edge, and the in-process plumbing that keeps it honest.
	//
	// gatewayLis is a bufconn: the gateway dials the gRPC server THROUGH IT rather than over
	// loopback TCP. Same interceptors, same server, no network hop, no self-connection
	// showing up in connection metrics, and nothing to misconfigure -- there is no address
	// for the gateway to point at the wrong thing.
	gatewayLis  *bufconn.Listener
	gatewayConn *grpc.ClientConn
	gatewaySrv  *http.Server

	// steps are torn down in reverse order by Shutdown.
	steps []Step
}

// New builds every dependency.
//
// It returns an error rather than panicking, so a wiring test can assert simply that New
// succeeds: a missing constructor argument or an unhandled driver name comes back as an
// error naming the reason. M4 will add that test.
//
// On a partial failure, everything already opened is closed before returning. Leaking a
// database pool because startup failed halfway is how a crash-looping pod exhausts a
// database's connection limit.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	if log == nil {
		log = observability.NewLogger(cfg, os.Stdout)
	}

	a := &App{cfg: cfg, log: log}

	// The verifier is built HERE, in the composition root, and passed down.
	//
	// auth.NewVerifier owns the AUTH_MODE switch, including the unknown-mode arm that
	// returns an error and the dev-mode warning emitted on every startup. Building it early
	// means a bad OIDC configuration -- missing audience, http:// issuer, absent tenant
	// claim -- fails here, before any listener binds, rather than on the first request.
	verifier, err := auth.NewVerifier(cfg, log)
	if err != nil {
		return nil, fmt.Errorf("build verifier: %w", err)
	}

	// Tracing first: it must be installed before anything that creates spans, and the
	// propagator must be set even when no exporter is configured, or this service silently
	// breaks trace continuity for every service downstream of it.
	_, flushTraces, err := observability.NewTracerProvider(ctx, cfg)
	if err != nil {
		_ = a.closeOpened(ctx)
		return nil, fmt.Errorf("tracing: %w", err)
	}
	a.steps = append(a.steps, Step{
		Name:    "flush-traces",
		Timeout: 5 * time.Second,
		Fn:      flushTraces,
	})

	a.metrics = observability.NewMetrics()

	// The validator compiles every CEL rule in every registered proto ONCE, at startup.
	//
	// Building it per-request would recompile them on every call. Building it here also
	// means a malformed rule fails startup with a clear error instead of turning into an
	// Internal on whichever RPC happens to be hit first in production.
	validator, err := protovalidate.New()
	if err != nil {
		_ = a.closeOpened(ctx)
		return nil, fmt.Errorf("build protovalidate: %w", err)
	}

	store, atomic, err := a.buildStore(ctx)
	if err != nil {
		_ = a.closeOpened(ctx)
		return nil, err
	}

	orderSvc := order.NewService(store, atomic)

	a.health = health.NewServer()
	a.grpcServer, err = grpcapi.NewServer(grpcapi.Deps{
		Log:          log,
		Cfg:          cfg,
		OrderService: orderSvc,
		Health:       a.health,
		Metrics:      a.metrics,
		Validator:    validator,
		Verifier:     verifier,
		Policy:       grpcapi.DefaultPolicy(),
	})
	if err != nil {
		_ = a.closeOpened(ctx)
		return nil, err
	}

	a.adminSrv = a.buildAdminServer()

	if err := a.buildGateway(ctx); err != nil {
		_ = a.closeOpened(ctx)
		return nil, fmt.Errorf("gateway: %w", err)
	}

	return a, nil
}

// buildGateway wires the REST edge onto the gRPC server through an in-process connection.
//
// GATEWAY_ADDR empty means no REST surface at all, and nothing here runs.
func (a *App) buildGateway(ctx context.Context) error {
	if a.cfg.GatewayAddr == "" {
		a.log.Info("REST gateway disabled", slog.String("hint", "set GATEWAY_ADDR to enable it"))
		return nil
	}

	// A second listener for the SAME grpc.Server. grpc.Server.Serve may be called on any
	// number of listeners concurrently, so this costs one goroutine and no duplicated wiring:
	// the gateway reaches the identical server, with the identical chain.
	a.gatewayLis = bufconn.Listen(gatewayBufSize)

	served := make(chan struct{})
	go func() {
		defer close(served)
		// ErrServerStopped on shutdown is expected and not worth logging.
		if err := a.grpcServer.Serve(a.gatewayLis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			a.log.Error("in-process gRPC listener stopped", slog.String("error", err.Error()))
		}
	}()

	// "passthrough:///" is mandatory. grpc.NewClient runs the target through the DNS
	// resolver by default, so a bare name becomes a lookup that fails on a machine with a
	// wildcard resolver and hangs on one without.
	conn, err := grpc.NewClient("passthrough:///gateway",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return a.gatewayLis.DialContext(ctx)
		}),
		// insecure is CORRECT here and nowhere else: this connection never leaves the
		// process, so there is no transport to secure. TLS on a bufconn would be ceremony
		// with a real CPU cost on every request.
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("dial in-process: %w", err)
	}
	a.gatewayConn = conn

	mux, err := gateway.NewMux(ctx, conn)
	if err != nil {
		return fmt.Errorf("build mux: %w", err)
	}

	a.gatewaySrv = &http.Server{
		Addr:    a.cfg.GatewayAddr,
		Handler: gateway.Handler(mux),

		// Timeouts are set explicitly because net/http's defaults are all zero, meaning no
		// timeout at all: a client that opens a connection and never sends a byte holds it
		// forever, and enough of them exhaust the file descriptor limit. This is the HTTP
		// equivalent of the keepalive enforcement on the gRPC side.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// No WriteTimeout: WatchOrders is a server-streaming RPC transcoded to a long-lived
		// HTTP response, and a write deadline would cut it off mid-stream. The request
		// deadline interceptor bounds unary work instead.
		IdleTimeout: 120 * time.Second,
	}

	a.steps = append(a.steps,
		Step{
			Name:    "gateway-conn",
			Timeout: 5 * time.Second,
			Fn:      func(context.Context) error { return conn.Close() },
		},
		Step{
			Name:    "gateway-listener",
			Timeout: 5 * time.Second,
			Fn: func(context.Context) error {
				err := a.gatewayLis.Close()
				<-served
				return err
			},
		},
	)

	return nil
}

// gatewayBufSize is the in-process connection buffer. Large enough that a 4 MiB message --
// the configured gRPC maximum -- never blocks on the buffer itself.
const gatewayBufSize = 8 << 20

// GatewayHandler exposes the REST mux so tests can drive it with httptest, without binding a
// port. Nil when GATEWAY_ADDR is empty.
func (a *App) GatewayHandler() http.Handler {
	if a.gatewaySrv == nil {
		return nil
	}
	return a.gatewaySrv.Handler
}

// Metrics exposes the registry so tests can scrape it without a listener.
func (a *App) Metrics() *observability.Metrics { return a.metrics }

// buildStore selects the persistence driver.
func (a *App) buildStore(ctx context.Context) (order.Store, order.Atomic, error) {
	switch a.cfg.StoreDriver {
	case config.StoreMemory:
		mem := ordermem.New()

		// Seed only the in-memory driver. A service that writes fixture rows into a real
		// database on boot will eventually do it to production.
		if err := order.Seed(ctx, mem, time.Now()); err != nil {
			return nil, nil, fmt.Errorf("seed in-memory store: %w", err)
		}
		a.log.Info("using the in-memory store",
			slog.String("tenant", order.SeedTenant),
			slog.String("hint", "set STORE_DRIVER=postgres and POSTGRES_DSN for a real database"))
		return mem, mem, nil

	case config.StorePostgres:
		db, err := gormx.Open(ctx, a.cfg, a.log)
		if err != nil {
			return nil, nil, fmt.Errorf("open postgres: %w", err)
		}

		// Registered as a shutdown step so the pool closes on the way out. Without it a
		// crash-looping pod leaks its connections until the server times them out, and a
		// database with a modest max_connections runs out long before the pod stabilises.
		sqlDB, err := db.DB()
		if err != nil {
			return nil, nil, fmt.Errorf("unwrap sql.DB: %w", err)
		}
		a.steps = append(a.steps, Step{
			Name:    "postgres-pool",
			Timeout: 5 * time.Second,
			Fn:      func(context.Context) error { return sqlDB.Close() },
		})

		// NO MIGRATIONS HERE, and no seeding either.
		//
		// Migrations run only from cmd/migrate; see that file for why booting them from every
		// replica turns a slow ALTER into a stalled rollout. Seeding is skipped because a
		// service that writes fixture rows into a real database on boot will eventually do it
		// to production.
		store := orderpg.New(db)
		a.log.Info("using the postgres store",
			slog.Int("max_open_conns", a.cfg.Postgres.MaxOpenConns),
			slog.String("hint", "run `go run ./cmd/migrate up` if the schema is not current"))
		return store, store, nil

	default:
		return nil, nil, fmt.Errorf("unknown STORE_DRIVER %q", a.cfg.StoreDriver)
	}
}

// buildAdminServer serves metrics, pprof and health on a PRIVATE listener.
//
// This split is a security control, not tidiness. net/http/pprof registers its handlers
// on http.DefaultServeMux from an init() function simply by being imported, anywhere in
// the dependency tree. If the public mux is DefaultServeMux, importing a library that
// transitively imports pprof publishes a heap dumper and a CPU-profiler trigger on your
// ingress. Using an explicit mux here, bound to a private address, closes that by
// construction. admin_test.go asserts the public listeners do not serve /debug/pprof.
func (a *App) buildAdminServer() *http.Server {
	// live reports process health only -- deliberately NOT dependency health.
	//
	// A liveness probe that fails when Postgres is down makes Kubernetes restart every
	// replica at once during a database blip, turning a recoverable dependency outage into
	// a self-inflicted total outage. Readiness (grpc.health.v1, on the RPC port) is what
	// gates traffic.
	live := func() bool { return true }

	mux := observability.NewAdminMux(a.metrics.Registry, live)
	return observability.NewAdminServer(a.cfg, mux)
}

// AdminHandler exposes the admin mux so tests can exercise /metrics and /debug/pprof
// without binding a port -- and so admin_test.go can assert the PUBLIC surfaces do not
// serve them.
func (a *App) AdminHandler() http.Handler { return a.adminSrv.Handler }

// GRPCServer exposes the wired server so tests can serve it over an in-memory listener.
func (a *App) GRPCServer() *grpc.Server { return a.grpcServer }

// Health exposes the health server so tests can flip serving status.
func (a *App) Health() *health.Server { return a.health }

// MarkServing reports the service as healthy.
//
// Run calls this once its listeners are bound. It is exported so an in-process harness
// serving over bufconn -- which never calls Run -- can reach the same state, instead of
// every health assertion seeing the zero value and failing for the wrong reason.
func (a *App) MarkServing() {
	a.health.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
}

// Run binds the listeners and blocks until ctx is cancelled, then drains.
func (a *App) Run(ctx context.Context) error {
	// ListenConfig rather than net.Listen: it takes a context, so a shutdown signal
	// arriving mid-bind cancels the bind instead of leaving a socket open that nothing
	// will ever close.
	var lc net.ListenConfig
	grpcLn, err := lc.Listen(ctx, "tcp", a.cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", a.cfg.GRPCAddr, err)
	}

	// Empty service name is the overall status, which is what a Kubernetes grpc: probe
	// with no `service` field checks.
	a.health.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	errCh := make(chan error, 3)

	go func() {
		a.log.Info("gRPC listening", slog.String("addr", grpcLn.Addr().String()))
		if err := a.grpcServer.Serve(grpcLn); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			errCh <- fmt.Errorf("grpc serve: %w", err)
		}
	}()

	go func() {
		a.log.Info("admin listening", slog.String("addr", a.adminSrv.Addr))
		if err := a.adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("admin serve: %w", err)
		}
	}()

	if a.gatewaySrv != nil {
		go func() {
			a.log.Info("REST gateway listening", slog.String("addr", a.gatewaySrv.Addr))
			if err := a.gatewaySrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("gateway serve: %w", err)
			}
		}()
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		a.log.Info("shutdown signal received")
		return a.Close(context.WithoutCancel(ctx))
	}
}

// Close drains, stops serving, and releases resources.
//
// The sequence is deliberately explicit rather than derived, because the drain has to
// happen BEFORE anything stops accepting work, which is not something reverse
// construction order would ever give you.
func (a *App) Close(ctx context.Context) error {
	// Step 1: stop being routable, but keep serving.
	//
	// Kubernetes removes a pod from Service endpoints asynchronously with delivering
	// SIGTERM. A pod that stops instantly on SIGTERM still receives requests for a short
	// window and answers them with connection refused -- which the client sees as a 503
	// during every single deploy. Flipping health first and then WAITING is what makes a
	// rolling update invisible to callers.
	a.health.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	a.log.Info("health set to NOT_SERVING, draining", slog.Duration("delay", a.cfg.Shutdown.DrainDelay))
	select {
	case <-time.After(a.cfg.Shutdown.DrainDelay):
	case <-ctx.Done():
	}

	// Step 2: everything else, innermost-last.
	//
	// a.steps holds resource closers in CONSTRUCTION order (database opened before the
	// server, and so on). Shutdown iterates backwards, so they release in the reverse of
	// the order they were acquired -- the server stops before the pool it depends on.
	steps := append([]Step(nil), a.steps...)

	// The GATEWAY stops before the gRPC server it depends on.
	//
	// Reversed, an in-flight HTTP request would find its in-process connection closed
	// underneath it and return a 500 for a request that was about to succeed -- during every
	// deploy. This is the same "drain outside-in" reasoning as the health flip above, applied
	// one layer down.
	if a.gatewaySrv != nil {
		steps = append(steps, Step{
			Name:    "gateway-server",
			Timeout: 5 * time.Second,
			Fn:      func(ctx context.Context) error { return a.gatewaySrv.Shutdown(ctx) },
		})
	}

	steps = append(steps,
		Step{
			Name:    "admin-server",
			Timeout: 5 * time.Second,
			Fn:      func(ctx context.Context) error { return a.adminSrv.Shutdown(ctx) },
		},
		Step{
			Name:    "grpc-server",
			Timeout: a.cfg.Shutdown.GracePeriod,
			Fn: func(ctx context.Context) error {
				done := make(chan struct{})
				go func() {
					a.grpcServer.GracefulStop()
					close(done)
				}()
				select {
				case <-done:
					return nil
				case <-ctx.Done():
					// Grace period exhausted. Cut the remaining connections rather than
					// hanging past terminationGracePeriodSeconds and being SIGKILLed
					// mid-write, which is strictly worse for the client.
					a.log.Warn("graceful stop timed out, forcing")
					a.grpcServer.Stop()
					return nil
				}
			},
		},
	)

	return Shutdown(ctx, steps)
}

// closeOpened releases anything already constructed when New fails partway.
func (a *App) closeOpened(ctx context.Context) error {
	return Shutdown(ctx, a.steps)
}
