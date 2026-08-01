// Command devtool is the cross-platform half of the task runner.
//
// Taskfile.yml may only invoke go, docker, kubectl and git. That restriction is not
// stylistic: Task's embedded shell (mvdan.cc/sh) provides no rm, cp, mv, sed, awk or jq
// builtins on Windows, so a Taskfile that shells out to them works on a maintainer's Mac
// and fails on a contributor's laptop with an error that names none of this.
//
// Everything filesystem- or text-shaped therefore lives here, in Go, where it is
// genuinely portable. test/taskfile_test.go enforces the rule.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx := context.Background()

	var err error
	switch os.Args[1] {
	case "doctor":
		err = doctor(ctx)
	case "clean":
		err = clean(ctx)
	case "race":
		err = race(ctx)
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "devtool: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: devtool <command>

  doctor   report what this machine can run, and why
  clean    remove build and test artifacts
  race     run the race detector, or explain why it cannot run here`)
}

// clean removes build output. In Go rather than `rm -rf` so it behaves the same on
// Windows, and scoped to known paths so a typo cannot delete the repository.
func clean(ctx context.Context) error {
	targets := []string{"bin", "covdata", "coverage.out", "coverage.html"}
	for _, t := range targets {
		if err := os.RemoveAll(t); err != nil {
			return fmt.Errorf("remove %s: %w", t, err)
		}
	}

	// The build and test caches are owned by the toolchain, so ask the toolchain.
	cmd := exec.CommandContext(ctx, "go", "clean", "-testcache")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go clean -testcache: %w", err)
	}

	fmt.Println("cleaned: bin/, covdata/, coverage files, test cache")
	return nil
}

// race runs the race detector, or explains precisely why it cannot.
//
// `go test -race` requires cgo, and cgo requires a C toolchain. On a stock Windows
// install there is no gcc, so the raw failure is `-race requires cgo` or
// `cgo: C compiler "gcc" not found` -- neither of which tells a newcomer what to do, and
// both of which look like the template is broken.
//
// This is a permanent limitation of the race detector (it links the C++ ThreadSanitizer
// runtime), not a configuration gap, so the honest move is to detect it and offer the two
// real options rather than emit a confusing toolchain error.
func race(ctx context.Context) error {
	if _, err := exec.LookPath("gcc"); err != nil {
		if runtime.GOOS == "windows" {
			return errors.New(strings.TrimSpace(`
the race detector needs cgo, and no C compiler was found on PATH.

This is expected on a stock Windows install and is not a bug in this template. The race
detector links the C++ ThreadSanitizer runtime, so it cannot work without a C toolchain.

Two options:

  1. Run it in a container (no install, slower):
       docker run --rm -v "${PWD}:/src" -w /src golang:1.26 go test -race ./...

  2. Install a C toolchain natively (recommended if you develop here daily):
       winget install BrechtSanders.WinLibs.POSIX.UCRT
       go env -w CGO_ENABLED=1
     Note that "go env -w" is GLOBAL for your user and affects every other Go project.

CI runs -race on ubuntu-latest on every push, so race coverage is not lost -- the
windows-latest CI leg proves compilation and non-race correctness only.`))
		}
		return errors.New("no C compiler found on PATH; the race detector requires cgo")
	}

	cmd := exec.CommandContext(ctx, "go", "test", "-race", "./...")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	return cmd.Run()
}

// doctor reports what this machine can run. It never fails the build -- a machine without
// Docker is a perfectly good machine for the default test tier, and saying so plainly is
// more useful than a red X.
func doctor(ctx context.Context) error {
	fmt.Printf("go            %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	reportTool("docker", "integration and e2e tiers")
	reportTool("kubectl", "task k8s:render")
	reportTool("grpcurl", "manual gRPC poking (optional)")

	if _, err := exec.LookPath("gcc"); err != nil {
		fmt.Println("cgo           NOT available -- `task test:race` will explain the options")
	} else {
		fmt.Println("cgo           available -- `task test:race` works natively")
	}

	// The Docker context trap.
	//
	// testcontainers-go resolves the daemon via ~/.testcontainers.properties, then
	// DOCKER_HOST, then the DEFAULT named pipe (docker_engine). It does NOT read docker
	// contexts. On Docker Desktop the active context is usually desktop-linux, whose pipe
	// is dockerDesktopLinuxEngine -- so a perfectly healthy daemon can be invisible to
	// testcontainers, and the integration tier skips for a reason nobody can guess.
	if out, err := exec.CommandContext(ctx, "docker", "context", "show").Output(); err == nil {
		ctxName := strings.TrimSpace(string(out))
		fmt.Printf("docker ctx    %s\n", ctxName)
		if ctxName != "default" && os.Getenv("DOCKER_HOST") == "" {
			fmt.Println("              note: testcontainers ignores docker contexts and probes the")
			fmt.Println("              default pipe. If the integration tier skips while docker works,")
			fmt.Println("              set DOCKER_HOST to this context's endpoint.")
		}
	}

	wd, _ := os.Getwd()
	if strings.Contains(strings.ToLower(wd), string(filepath.Separator)+"onedrive"+string(filepath.Separator)) {
		fmt.Println()
		fmt.Println("WARNING: this repository is inside OneDrive.")
		fmt.Println("  OneDrive's sync filter driver causes intermittent 'Access is denied' during")
		fmt.Println("  builds and git checkouts, and syncing .git across machines corrupts the index.")
		fmt.Println("  Move the repository outside OneDrive.")
	}

	fmt.Println()
	fmt.Println("test tiers:")
	fmt.Println("  task verify       always runs (no Docker, no network, no cgo)")
	fmt.Println("  task verify:int   needs Docker")
	fmt.Println("  task verify:e2e   needs Docker, builds the real image")
	return nil
}

func reportTool(name, purpose string) {
	if path, err := exec.LookPath(name); err == nil {
		fmt.Printf("%-13s %s\n", name, path)
	} else {
		fmt.Printf("%-13s NOT FOUND -- needed for: %s\n", name, purpose)
	}
}
