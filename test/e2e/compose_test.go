//go:build e2e

// Package e2e runs the SHIPPED artifacts: the real Dockerfile, the real compose file, the real
// images. Everything else in this repository tests the code; this tests what is deployed.
//
// The distinction is not academic. Every finding this tier has produced was invisible to the
// other tiers because it lived in a file no Go test reads:
//
//   - The compose healthcheck ran `/orderd --help`, a flag the binary never parsed. Instead of
//     printing usage it started a second server inside the container and raced the first for
//     the port, so the container could never report healthy.
//
// A repository whose deployment story is verified by hand once has a deployment story that was
// true once.
//
// # What it costs, measured on the development machine
//
//	docker build, warm cache   16s   (three targets)
//	compose up --wait          ~25s
//	the suite                  ~90s total
//
// Behind //go:build e2e because it needs a Docker daemon and minutes, not because it is
// optional. `task verify:e2e` runs it.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// project isolates this stack from a developer's own.
//
// Compose derives container names, the network and volume names from it, so a distinct project
// means `task up` can be running in the same checkout without the two colliding -- everywhere
// except published host ports, which are a single global namespace. See requireFreePorts.
const project = "gomicro-e2e"

const composeFile = "../../deploy/compose/docker-compose.yml"

// TestMain builds the images, brings the stack up, and tears it down once.
//
// Once, not per test: `compose up` is the dominant cost here, and a per-test stack would turn a
// ninety-second suite into a twenty-minute one. The trade is that tests share state, so each one
// below creates the data it asserts on rather than assuming a fixture.
func TestMain(m *testing.M) {
	if err := requireDocker(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(0) // skip, not fail: a machine without Docker has not broken anything
	}

	// TEAR DOWN FIRST, then check ports.
	//
	// The order matters and the first version had it backwards: the port check fired on this
	// suite's OWN leftovers -- from a previous run, or one killed mid-test -- and told the
	// developer to stop a dev stack that was not running. Removing our project first means the
	// only thing a port collision can now mean is what the message says.
	down()

	if err := requireFreePorts(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	code := 1
	func() {
		defer down()

		if err := up(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: bringing the stack up failed: %v\n", err)
			dumpLogs()
			return
		}
		code = m.Run()
		if code != 0 {
			dumpLogs()
		}
	}()

	os.Exit(code)
}

// up builds and starts the app profile, and waits for health.
//
// --wait is the assertion, not a convenience. It blocks until every service with a healthcheck
// reports healthy and every one-shot service has exited zero, so a broken healthcheck or a
// migration that fails is a startup error here rather than a confusing timeout inside a test.
func up() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	out, err := compose(ctx, "--profile", "app", "up", "--build", "--wait", "--wait-timeout", "180")
	if err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

func down() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// -v removes the volumes too. Leaving a populated database behind would make the next run
	// pass or fail depending on what the previous one did, which is the worst property a test
	// suite can have.
	if out, err := compose(ctx, "--profile", "app", "down", "-v", "--remove-orphans"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: teardown failed: %v\n%s\n", err, out)
	}
}

// compose runs docker compose against the shipped file, under this suite's own project name.
func compose(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"compose", "-f", composeFile, "-p", project}, args...)

	cmd := exec.CommandContext(ctx, "docker", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %w", strings.Join(full, " "), err)
	}
	return string(out), nil
}

// composeIn runs a command inside a running service container.
func composeExec(t *testing.T, service string, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	out, err := compose(ctx, append([]string{"exec", "-T", service}, args...)...)
	if err != nil {
		t.Fatalf("exec in %s: %v\n%s", service, err, out)
	}
	return out
}

// requireDocker reports why the suite cannot run, rather than failing obscurely.
func requireDocker() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		return fmt.Errorf("no Docker daemon; skipping the e2e tier (%v)", err)
	}
	return nil
}

// requireFreePorts fails EARLY and specifically on the one collision a separate project name
// cannot prevent.
//
// Published host ports are global. A developer running `task up` in this same checkout has
// these bound, and without this check the failure arrives as a compose error about port
// allocation buried in build output, several minutes in.
//
// THIS LIST HAS TO TRACK THE COMPOSE FILE. 9091 was added when the worker gained an admin
// listener; forgetting it would have reintroduced exactly the buried failure this exists to
// prevent, for the one service whose port is newest and least expected.
func requireFreePorts() error {
	for _, port := range []string{"50051", "8080", "9091"} {
		if inUse(port) {
			return fmt.Errorf("host port %s is already bound.\n\n"+
				"The e2e stack publishes the same ports the compose file does, so a dev stack "+
				"has to stop first:\n\n    task down\n", port)
		}
	}
	return nil
}

func dumpLogs() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	out, _ := compose(ctx, "--profile", "app", "logs", "--tail", "80")
	fmt.Fprintf(os.Stderr, "\n=== compose logs ===\n%s\n", out)
}
