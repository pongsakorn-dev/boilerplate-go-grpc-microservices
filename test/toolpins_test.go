package test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/example/gomicro/internal/testutil"
)

// bannedTools must never appear in the go.mod `tool` directive.
var bannedTools = []struct{ needle, why string }{
	{
		needle: "bufbuild/buf",
		why: "Buf's own docs say not to install it via tools.go or `go tool`, because that\n" +
			"resolves buf's dependencies against yours. Both buf and this service depend on\n" +
			"google.golang.org/protobuf and golang.org/x/net, so MVS would silently upgrade\n" +
			"your protobuf RUNTIME to satisfy a build tool. The resulting panic would never\n" +
			"be traced back to the tool directive.\n" +
			"Use `go run github.com/bufbuild/buf/cmd/buf@vX.Y.Z` instead -- the @version\n" +
			"suffix makes go run ignore this module's go.mod entirely.",
	},
	{
		needle: "golangci-lint",
		why: "golangci-lint's docs disclaim the tool directive for the same MVS reason.\n" +
			"Use `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@vX.Y.Z`.",
	},
}

// TestBannedToolsAreNotToolDependencies protects the dependency graph of every service
// built from this template.
//
// This is the highest-leverage guard in the repository, because the failure it prevents is
// completely invisible: adding buf to the tool directive does not error, does not warn, and
// produces a working build -- it just quietly moves your protobuf runtime version. The
// symptom appears months later as a runtime panic in a service nobody connected to a
// Makefile-equivalent change.
func TestBannedToolsAreNotToolDependencies(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("go", "tool")
	cmd.Dir = testutil.RepoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go tool: %v", err)
	}

	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, banned := range bannedTools {
			if strings.Contains(line, banned.needle) {
				t.Errorf("%q is in the go.mod tool directive.\n\n%s", line, banned.why)
			}
		}
	}
}

// bufPinRE matches the pinned buf module path anywhere it appears.
var bufPinRE = regexp.MustCompile(`github\.com/bufbuild/buf/cmd/buf@(v[0-9]+\.[0-9]+\.[0-9]+)`)

// pinnedFiles are every place the buf version is written down.
var pinnedFiles = []string{
	"Taskfile.yml",
	".github/workflows/ci.yml",
	"test/codegen_uptodate_test.go",
}

// TestBufVersionIsConsistent stops the three copies of the buf pin from drifting.
//
// The version has to be repeated because each consumer resolves it differently: Task
// interpolates a variable, GitHub Actions cannot read that variable, and the codegen test
// execs buf itself. Three copies is the cost of not putting buf in the tool directive.
//
// Drift here is nastier than it looks. If CI pins a different buf than the codegen test
// does, generated output differs between the two, and `TestGeneratedCodeIsUpToDate` fails
// in CI with a diff that cannot be reproduced locally -- the worst category of CI failure.
func TestBufVersionIsConsistent(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)
	found := map[string]string{}

	for _, rel := range pinnedFiles {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s must exist and pin the buf version: %v", rel, err)
		}
		m := bufPinRE.FindStringSubmatch(string(b))
		if m == nil {
			t.Errorf("%s does not pin github.com/bufbuild/buf/cmd/buf@vX.Y.Z", rel)
			continue
		}
		found[rel] = m[1]
	}

	versions := map[string][]string{}
	for file, v := range found {
		versions[v] = append(versions[v], file)
	}

	if len(versions) > 1 {
		var lines []string
		for v, files := range versions {
			sort.Strings(files)
			lines = append(lines, "  "+v+"  <- "+strings.Join(files, ", "))
		}
		sort.Strings(lines)
		t.Errorf("the buf version has drifted between files:\n%s\n\n"+
			"All three must match, or CI and local codegen produce different output.",
			strings.Join(lines, "\n"))
	}
}

// TestPinnedToolsBuild proves every tool in the directive is actually usable.
//
// A tool that is pinned but does not compile fails at `task gen` time, which is the worst
// possible moment: a cloner hits it on their first attempt to change a proto and has no
// idea whether they caused it.
func TestPinnedToolsBuild(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("compiles each pinned tool; skipped under -short")
	}

	root := testutil.RepoRoot(t)

	cmd := exec.Command("go", "tool")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go tool: %v", err)
	}

	var tools []string
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		// Built-in toolchain commands (compile, link, vet, ...) have no slash.
		if line != "" && strings.Contains(line, "/") {
			tools = append(tools, line)
		}
	}
	if len(tools) == 0 {
		t.Fatal("no module tools found in the tool directive -- this guard would silently pass")
	}

	for _, tool := range tools {
		t.Run(tool, func(t *testing.T) {
			t.Parallel()
			build := exec.Command("go", "build", "-o", filepath.Join(t.TempDir(), "out"), tool)
			build.Dir = root
			if out, err := build.CombinedOutput(); err != nil {
				t.Errorf("pinned tool %s does not build: %v\n%s", tool, err, out)
			}
		})
	}
}

// TestGoModIsTidy catches manifest drift, which is invisible until it misleads somebody.
//
// # What actually went wrong
//
// This repository ran for eleven milestones with `gorm.io/gorm`, `gorm.io/driver/postgres`,
// `github.com/jackc/pgx/v5`, `github.com/testcontainers/testcontainers-go`,
// `github.com/nats-io/nats.go`, `github.com/nats-io/nats-server/v2`,
// `github.com/pressly/goose/v3`, `github.com/redis/go-redis/v9`,
// `github.com/alicebob/miniredis/v2` and `github.com/golang-jwt/jwt/v5` all marked `// indirect`
// in go.mod -- while being imported directly by its own code. Nothing detected it. It surfaced
// only because an unrelated change needed `go mod tidy`, which then swept up twenty-four lines
// of unrelated churn into a test-backfill diff.
//
// The `// indirect` marker is what a reader uses to answer "what did WE choose to depend on?",
// which is the first question in any dependency review and the one this repo's own rules turn
// on -- see TestBannedToolsAreNotToolDependencies above, and the README's argument for keeping
// buf and go-task out of the module graph. A manifest where that marker lies makes the answer
// wrong for everybody who reads it afterwards.
//
// # Why -diff rather than running tidy and diffing by hand
//
// `go mod tidy -diff` computes exactly what tidy would write and exits non-zero without
// touching anything, so this guard cannot leave a modified go.mod behind on a failure -- which
// a copy-run-compare version would, on the run where it fails, in a working tree somebody is
// mid-change on.
func TestGoModIsTidy(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "mod", "tidy", "-diff")
	cmd.Dir = root

	// GOPROXY=off keeps this in the default tier honestly.
	//
	// tidy resolves the module graph, and against a cold cache it would reach the network --
	// which the default tier promises never to need. With the cache warm it does not, and with
	// the proxy off a cold cache fails loudly as a cache problem rather than silently becoming
	// a network test.
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=mod")

	out, err := cmd.CombinedOutput()
	if err == nil {
		return // tidy would change nothing
	}

	// A non-zero exit with no diff means tidy itself failed -- most likely a cold module
	// cache with the proxy off. That is a broken environment, not drift, and saying which
	// saves somebody a confusing half hour.
	if len(bytes.TrimSpace(out)) == 0 {
		t.Skipf("`go mod tidy -diff` could not run (%v); the module cache is probably cold. "+
			"Run `go mod download` and try again.", err)
	}

	t.Errorf("go.mod or go.sum is not tidy:\n\n%s\n\n"+
		"Run `go mod tidy`. The usual cause is a dependency that is used directly but still "+
		"marked `// indirect` -- which makes go.mod misreport what this repository actually "+
		"chose to depend on, and that marker is the first thing anyone reviewing dependencies "+
		"reads.", string(out))
}
