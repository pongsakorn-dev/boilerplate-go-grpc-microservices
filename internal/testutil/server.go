// Package testutil holds the shared test harness.
package testutil

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/example/gomicro/internal/app"
	"github.com/example/gomicro/internal/platform/config"
)

const bufSize = 1024 * 1024

// TestConfig is a valid configuration for an in-process server.
func TestConfig(mutate ...func(*config.Config)) config.Config {
	cfg, err := config.Parse(map[string]string{
		"APP_ENV":               config.EnvDevelopment,
		"SERVICE_NAME":          "orderd-test",
		"STORE_DRIVER":          config.StoreMemory,
		"AUTH_MODE":             config.AuthDev,
		"SHUTDOWN_DRAIN_DELAY":  "0s",
		"SHUTDOWN_GRACE_PERIOD": "2s",
	})
	if err != nil {
		panic("testutil.TestConfig is invalid: " + err.Error())
	}
	for _, fn := range mutate {
		fn(&cfg)
	}
	return cfg
}

// NewTestServer starts the PRODUCTION server over an in-memory listener and returns a
// connected client.
//
// bufconn rather than a real port, for three concrete reasons:
//   - No port allocation, so tests run in parallel with no chance of collision and no
//     flakes on a busy CI machine.
//   - No TCP stack, so a full round trip is microseconds.
//   - Nothing to leak: there is no socket left in TIME_WAIT.
//
// Critically, this builds the server via app.New and grpcapi.NewServer -- the same code
// paths production uses -- so the real interceptor chain, the real hardening options, and
// the real health and reflection registration are all under test. A harness that assembles
// its own grpc.Server tests a server that does not exist.
func NewTestServer(t *testing.T, mutate ...func(*config.Config)) *grpc.ClientConn {
	t.Helper()

	cfg := TestConfig(mutate...)

	// Discard logs by default: a passing test should be silent. Use NewTestServerWithLogs
	// when the log output is the thing under assertion.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	application, err := app.New(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	return serve(t, application)
}

// NewTestServerDialer starts the production server and returns the dial options needed to
// reach it, for a test that must build its OWN connection.
//
// NewTestServer hands back a finished *grpc.ClientConn, which is what almost every test wants
// and is useless to a test whose subject IS the client -- internal/platform/client has to
// apply its own interceptors, service config and credentials, so it needs the listener rather
// than a connection to it.
//
// The "passthrough:///" prefix belongs on the target the caller passes to grpc.NewClient:
// without it the target goes through the DNS resolver and the custom dialer below is never
// consulted.
func NewTestServerDialer(t *testing.T, mutate ...func(*config.Config)) []grpc.DialOption {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	application, err := app.New(context.Background(), TestConfig(mutate...), log)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	lis := bufconn.Listen(bufSize)
	srv := application.GRPCServer()
	application.MarkServing()

	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(lis)
	}()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = application.Close(ctx)

		<-served
		_ = lis.Close()
	})

	return []grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
	}
}

// NewTestServerWithLogs is NewTestServer with a caller-supplied logger, for tests that
// assert on log records.
func NewTestServerWithLogs(t *testing.T, log *slog.Logger, mutate ...func(*config.Config)) *grpc.ClientConn {
	t.Helper()

	application, err := app.New(context.Background(), TestConfig(mutate...), log)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	return serve(t, application)
}

func serve(t *testing.T, application *app.App) *grpc.ClientConn {
	t.Helper()

	lis := bufconn.Listen(bufSize)
	srv := application.GRPCServer()

	// Health must report SERVING or probe-style assertions see the zero value.
	application.MarkServing()

	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(lis)
	}()

	// The "passthrough:///" prefix is mandatory.
	//
	// grpc.NewClient runs the target through the DNS resolver by default, so a bare
	// "bufnet" becomes a DNS lookup that fails on a machine with a wildcard resolver and
	// hangs on one without. passthrough hands the string straight to the dialer, which is
	// what the custom dialer below expects.
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}

	t.Cleanup(func() {
		_ = conn.Close()

		// application.Close, NOT a hand-rolled GracefulStop.
		//
		// This used to stop only the gRPC server, which left everything else app.New had
		// built still running -- and app.New builds a gateway whenever GATEWAY_ADDR is set,
		// which TestConfig leaves at its default. The result was a *grpc.ClientConn and a
		// second bufconn listener leaked by EVERY call to NewTestServer, measured at six
		// goroutines per server with goleak. Nothing failed, because nothing was looking:
		// the only package running goleak (internal/order/ordermem) never starts a server.
		//
		// Closing the app also means the harness exercises the PRODUCTION shutdown sequence
		// -- health flip, drain, reverse-order steps -- rather than a simplified imitation of
		// it. TestConfig sets SHUTDOWN_DRAIN_DELAY=0s, so that costs no wall-clock time, and
		// Close already force-stops after the grace period, which is what the timeout here
		// used to do by hand.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = application.Close(ctx)

		<-served
		_ = lis.Close()
	})

	return conn
}

// NewTestGateway starts the REST edge over the PRODUCTION wiring and returns its base URL.
//
// It builds the whole app -- real interceptor chain, real gateway mux, real in-process gRPC
// connection -- and serves the resulting handler with httptest. No port for the gRPC side is
// bound: app.New already stands the gateway's bufconn listener up, so the transcoder is live
// the moment New returns.
//
// The point of driving the real app rather than assembling a mux here is the same as
// NewTestServer's: a harness that wires its own gateway tests a gateway that does not exist.
func NewTestGateway(t *testing.T, mutate ...func(*config.Config)) (*httptest.Server, *app.App) {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	application, err := app.New(context.Background(), TestConfig(mutate...), log)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	application.MarkServing()

	handler := application.GatewayHandler()
	if handler == nil {
		t.Fatal("the app has no gateway handler; GATEWAY_ADDR must be set for this harness")
	}

	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = application.Close(ctx)
	})

	return srv, application
}
