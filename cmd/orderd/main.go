// Command orderd runs the order service.
//
// main is deliberately tiny. Everything it could do -- wiring, listeners, shutdown --
// lives in internal/app, where it can be constructed and tested without a process. A main
// function that grows logic is a main function nothing can test.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/example/gomicro/internal/app"
	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/platform/observability"
)

// health makes the binary able to probe ITSELF.
//
// The image is distroless: no shell, no curl, no wget, and deliberately no grpc_health_probe --
// a second binary to keep patched inside an image whose whole point is that it holds one. So the
// server answers for itself.
//
// Kubernetes does not need this. It has had native `grpc:` probes since 1.27 and
// deploy/k8s/base/deployment.yaml uses them. Docker Compose has no equivalent, which is how the
// shipped compose healthcheck came to run `/orderd --help` -- a flag this binary never parsed,
// so instead of printing usage and exiting it started a SECOND server inside the container,
// raced the first one for the port, and could never report healthy.
//
// Nothing caught that, because nothing ran the stack. The e2e tier does.
var health = flag.Bool("health", false,
	"dial this service's own gRPC health endpoint, print the status, and exit 0 if SERVING")

func main() {
	flag.Parse()

	if *health {
		if err := checkHealth(); err != nil {
			fmt.Fprintf(os.Stderr, "unhealthy: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("SERVING")
		return
	}

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

// checkHealth dials the local gRPC port and asks grpc.health.v1.
//
// It reads the SAME configuration the server does, so moving the service to another port keeps
// it probeable without anyone remembering to update a health command in a second file.
func checkHealth() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	// ":50051" is a LISTEN address, not a dial address. Handing it straight to a dialler
	// works on some stacks and resolves to nothing on others, so the host is made explicit.
	target := cfg.GRPCAddr
	if strings.HasPrefix(target, ":") {
		target = "localhost" + target
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial %s: %w", target, err)
	}
	defer func() { _ = conn.Close() }()

	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		return fmt.Errorf("health check against %s: %w", target, err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		return fmt.Errorf("status is %s, not SERVING", resp.GetStatus())
	}
	return nil
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		// Configuration is validated before anything binds a port, so a bad deploy fails
		// in one second with a list of every problem, rather than crash-looping.
		return fmt.Errorf("configuration: %w", err)
	}

	log := observability.NewLogger(cfg, os.Stdout)

	// SIGTERM is what Kubernetes sends; os.Interrupt is Ctrl-C locally.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application, err := app.New(ctx, cfg, log)
	if err != nil {
		return err
	}

	return application.Run(ctx)
}
