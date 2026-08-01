package test

import (
	"os"
	"path/filepath"
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
		src := strings.ReplaceAll(string(b), "\r\n", "\n")

		idx := strings.Index(src, "//go:build")
		if idx < 0 {
			continue
		}

		// Everything from the constraint to the package clause must contain a blank line.
		pkgIdx := strings.Index(src, "\npackage ")
		if pkgIdx < 0 || pkgIdx < idx {
			t.Errorf("%s: //go:build appears after the package clause, so it is ignored", rel)
			continue
		}
		between := src[idx:pkgIdx]
		if !strings.Contains(between, "\n\n") {
			t.Errorf("%s: //go:build is not separated from `package` by a blank line, "+
				"so Go treats it as a plain comment and the file joins the DEFAULT build", rel)
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
// real network behaviour (internal/grpcapi/drain_test.go).
func TestSynctestIsNotUsedWithRealNetworking(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)
	networkImports := []string{
		`"net"`,
		`"net/http"`,
		`"google.golang.org/grpc"`,
		`grpc/test/bufconn`,
	}

	for _, rel := range testutil.TrackedFiles(t) {
		if !strings.HasSuffix(rel, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		src := string(b)

		if !strings.Contains(src, "synctest.") {
			continue
		}
		for _, imp := range networkImports {
			if strings.Contains(src, imp) {
				t.Errorf("%s uses testing/synctest AND imports %s.\n\n"+
					"A synctest bubble containing a real listener deadlocks or hangs, because\n"+
					"grpc-go's transport goroutines are not durably blocking. Test the pure\n"+
					"sequencing logic in a bubble and the network behaviour against a real\n"+
					"bufconn server, in two separate tests.", rel, imp)
			}
		}
	}
}
