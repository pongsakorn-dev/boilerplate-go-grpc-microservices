package test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/testutil"
)

// heavyTestDeps are dependencies that must never reach the DEFAULT test tier.
//
// Each one either needs a Docker daemon or costs meaningful compile time. The default tier
// has to stay runnable on a laptop with Docker stopped -- that is the tier a stranger runs
// first, and it is the one that decides whether they keep going.
var heavyTestDeps = []struct{ prefix, why string }{
	{"github.com/testcontainers/", "needs a Docker daemon"},
	{"github.com/nats-io/nats-server", "large; belongs behind //go:build integration"},
	{"github.com/ory/dockertest", "needs a Docker daemon"},
}

// TestDefaultTierNeedsNoDocker is the guard that keeps `go test ./...` honest.
//
// The subtlety this catches: testing.Short() is NOT sufficient. A t.Skip() guarded by
// -short still LINKS the heavy dependency into the test binary, so the package still fails
// to build if the dependency is broken, still pays the compile cost, and still appears in
// govulncheck. Only a build tag removes it from the build entirely.
//
// So the tiers are separated by build tags, and this test proves the default build really
// is free of them rather than merely skipping at run time.
func TestDefaultTierNeedsNoDocker(t *testing.T) {
	t.Parallel()

	deps := testutil.GoList(t, "-deps", "-test", "-f", "{{.ImportPath}}", "./...")
	if len(deps) == 0 {
		t.Fatal("go list returned nothing -- this guard would silently pass forever")
	}

	var violations []string
	for _, dep := range deps {
		for _, h := range heavyTestDeps {
			if strings.HasPrefix(dep, h.prefix) {
				violations = append(violations, "  "+dep+"  ("+h.why+")")
			}
		}
	}

	if len(violations) > 0 {
		t.Errorf("the default test tier links infrastructure dependencies:\n%s\n\n"+
			"Move the test behind a build tag:\n\n"+
			"    //go:build integration\n\n"+
			"    package foo\n\n"+
			"and run it with `go tool task verify:int`. Note that a testing.Short() skip is\n"+
			"NOT enough -- it still links the dependency into every test binary.",
			strings.Join(violations, "\n"))
	}
}

// TestTaggedTestsHaveAValidConstraint catches the silent-no-op typo.
//
// A //go:build line must be followed by a BLANK LINE before the package clause. Miss the
// blank line and Go treats it as an ordinary comment: the constraint is ignored, the file
// joins the default build, and the failure mode is the opposite of what the author
// intended -- an integration test that quietly runs (and fails) on a machine with no Docker.
//
// The compiler does not warn about this, so something has to.
func TestTaggedTestsHaveAValidConstraint(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)

	for _, rel := range testutil.TrackedFiles(t) {
		if !strings.HasSuffix(rel, ".go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}

		// Scan LINE BY LINE, and only above the package clause.
		//
		// A whole-file substring search is wrong here in a way that bites immediately:
		// this very file contains "//go:build" inside an error message, and a naive
		// search finds that literal and reports the file as broken. A build directive is
		// only meaningful before `package`, so that is the only region worth looking at.
		lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")

		pkgLine := -1
		for i, line := range lines {
			if strings.HasPrefix(line, "package ") {
				pkgLine = i
				break
			}
		}
		if pkgLine < 0 {
			continue
		}

		for i := 0; i < pkgLine; i++ {
			if !strings.HasPrefix(strings.TrimSpace(lines[i]), "//go:build") {
				continue
			}
			// The line immediately after the directive must be blank. Without the blank
			// line Go treats the directive as an ordinary comment: the constraint is
			// silently ignored, the file joins the default build, and an integration test
			// starts running on machines with no Docker. The compiler never warns.
			if i+1 >= pkgLine || strings.TrimSpace(lines[i+1]) != "" {
				t.Errorf("%s:%d: //go:build is not followed by a blank line, so Go treats "+
					"it as a plain comment and the file joins the DEFAULT build", rel, i+1)
			}
		}
	}
}

// TestSynctestIsNotUsedWithRealNetworking encodes a trap that costs an afternoon.
//
// testing/synctest runs a bubble of goroutines on a fake clock and requires every goroutine
// in the bubble to be durably blocking. grpc-go's transport goroutines block on operations
// the runtime does not classify that way, so a bubble containing a real listener either
// reports a spurious deadlock or hangs forever.
//
// A hanging test in the default tier is adoption-fatal: a newcomer sees `go test ./...`
// never return and has no way to tell whether they broke it.
//
// The rule: synctest for pure sequencing logic (internal/app/shutdown.go), real servers for
// real network behaviour.
func TestSynctestIsNotUsedWithRealNetworking(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)

	// Matched against the file's real IMPORT LIST, from the parsed AST -- not against its
	// raw text. Substring-scanning the source finds these paths inside string literals
	// (this file quotes every one of them in its own rules) and reports files that import
	// nothing of the sort.
	networkPackages := map[string]bool{
		"net":                                 true,
		"net/http":                            true,
		"google.golang.org/grpc":              true,
		"google.golang.org/grpc/test/bufconn": true,
	}

	fset := token.NewFileSet()

	for _, rel := range testutil.TrackedFiles(t) {
		if !strings.HasSuffix(rel, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(rel)), nil, 0)
		if err != nil {
			continue // a file that does not parse is a different test's problem
		}

		usesSynctest := false
		for _, imp := range file.Imports {
			if path, err := strconv.Unquote(imp.Path.Value); err == nil && path == "testing/synctest" {
				usesSynctest = true
				break
			}
		}
		if !usesSynctest {
			continue
		}

		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil || !networkPackages[path] {
				continue
			}
			t.Errorf("%s imports testing/synctest AND %s.\n\n"+
				"A synctest bubble containing a real listener deadlocks or hangs, because\n"+
				"grpc-go's transport goroutines are not durably blocking -- and a hanging\n"+
				"test in the default tier is worse than a failing one, because a newcomer\n"+
				"cannot tell whether they broke it.\n\n"+
				"Split it: test the pure sequencing logic in a bubble (see\n"+
				"internal/app/shutdown_test.go) and the real network behaviour against a\n"+
				"bufconn server in its own test.", rel, path)
		}
	}
}
