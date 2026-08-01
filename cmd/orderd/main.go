// Command orderd runs the order service.
//
// main is deliberately tiny. Everything it could do -- wiring, listeners, shutdown --
// lives in internal/app, where it can be constructed and tested without a process. A main
// function that grows logic is a main function nothing can test.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/example/gomicro/internal/app"
	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/platform/observability"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
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
