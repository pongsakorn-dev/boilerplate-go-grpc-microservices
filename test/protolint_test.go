//go:build codegen

package test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/testutil"
)

// TestBufLint keeps the API surface conventional.
//
// Consistency in an API is not cosmetic. The STANDARD rule set enforces the conventions
// every generated client library and every reviewer already expects: version-suffixed
// packages, Request/Response message naming, enum zero values suffixed _UNSPECIFIED, and
// enum values prefixed with the enum name. Diverging costs nothing on day one and costs a
// breaking change to fix on day ninety.
func TestBufLint(t *testing.T) {
	cmd := exec.Command("go", "run", bufPin, "lint")
	cmd.Dir = testutil.RepoRoot(t)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("buf lint failed:\n%s", out)
	}
}

// TestBufBreaking is the real contract test for a gRPC API.
//
// Consumer-driven contract tooling (Pact and similar) exists to solve a problem protobuf
// already solved: knowing whether a change breaks callers. A schema plus a breaking-change
// detector answers it statically, for every consumer including ones you have never heard
// of, with no broker to run and no pact files to keep current.
//
// buf.yaml sets `breaking.use: WIRE_JSON` explicitly rather than inheriting a default.
// That is deliberate: this template ships a JSON/REST edge, so a change that is
// wire-compatible but renames a JSON field still breaks HTTP callers.
func TestBufBreaking(t *testing.T) {
	root := testutil.RepoRoot(t)

	// A fresh clone with no main ref, or a repository whose first commit is the current
	// one, has nothing to compare against. Skipping is correct -- failing would make the
	// very first CI run red for a reason the author cannot act on.
	check := exec.Command("git", "rev-parse", "--verify", "main")
	check.Dir = root
	if err := check.Run(); err != nil {
		t.Skip("no `main` ref to compare against")
	}

	cmd := exec.Command("go", "run", bufPin, "breaking", "--against", ".git#branch=main")
	cmd.Dir = root

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("the proto API broke against main:\n%s\n\n"+
			"If the break is intentional, it needs a new package version (order/v2), not an\n"+
			"edit to v1 -- existing clients do not redeploy just because you did.",
			strings.TrimSpace(string(out)))
	}
}
