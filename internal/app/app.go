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

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/example/gomicro/internal/grpcapi"
	"github.com/example/gomicro/internal/order"
	"github.com/example/gomicro/internal/order/ordermem"
	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/platform/observability"
)

// App is a fully wired service, not yet listening.
type App struct {
	cfg config.Config
	log *slog.Logger

	grpcServer *grpc.Server
	health     *health.Server
	adminSrv   *http.Server

	// steps are torn down in reverse order by Shutdown.
	steps []Step
}

// New builds every dependency.
//
// It returns an error rather than panicking, which makes app_test.go's single assertion
// -- that New succeeds -- a genuine wiring test: if a constructor is missing an argument
// or a driver name is unhandled, this returns an error and the test fails with the reason.
//
// On a partial failure, everything already opened is closed before returning. Leaking a
// database pool because startup failed halfway is how a crash-looping pod exhausts a
// database's connection limit.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	if log == nil {
		log = observability.NewLogger(cfg, os.Stdout)
	}

	a := &App{cfg: cfg, log: log}

	// A dev-mode verifier accepts anything. Warn on EVERY startup, not once: a single
	// line at first boot scrolls out of a log buffer within minutes, and the whole point
	// is that someone notices it in an environment where it should not appear.
	if cfg.AuthMode == config.AuthDev {
		log.Warn("AUTH_MODE=dev: every request is accepted without verification. " +
			"Never run this outside local development.")
	}

	store, atomic, err := a.buildStore(ctx)
	if err != nil {
		_ = a.closeOpened(ctx)
		return nil, err
	}

	orderSvc := order.NewService(store, atomic)

	a.health = health.NewServer()
	a.grpcServer = grpcapi.NewServer(grpcapi.Deps{
		Log:          log,
		Cfg:          cfg,
		OrderService: orderSvc,
		Health:       a.health,
	})

	a.adminSrv = a.buildAdminServer()

	return a, nil
}

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
		// Wired in M4. Returning an explicit error rather than falling through to a nil
		// store means the failure names itself instead of panicking on first use.
		return nil, nil, errors.New("STORE_DRIVER=postgres is not wired yet (see milestone M4)")

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
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// M3 adds /metrics and /debug/pprof here.

	return &http.Server{
		Addr:              a.cfg.AdminAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

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

	errCh := make(chan error, 2)

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
