// Package testutil holds the shared test harness.
package testutil

import (
	"context"
	"io"
	"log/slog"
	"net"
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

		stopped := make(chan struct{})
		go func() {
			srv.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			// A test that leaves a stream open would otherwise hang the whole package.
			srv.Stop()
		}

		<-served
		_ = lis.Close()
	})

	return conn
}
