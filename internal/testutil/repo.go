package testutil

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// RepoRoot returns the repository root.
//
// Tests run with the working directory set to their own package, so a guard test that
// wants to inspect the whole tree has to find the root itself. Walking up to the directory
// containing go.mod is reliable regardless of how deep the test package sits.
func RepoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found walking up from the test's working directory")
		}
		dir = parent
	}
}

// GoList runs `go list` with the given arguments from the repo root and returns the
// non-empty output lines.
//
// Shelling out to the toolchain rather than importing golang.org/x/tools/go/packages is a
// deliberate trade. go/packages is the "proper" API, but it drags a large dependency tree
// into go.sum -- and in a single-module template that tree lands in every service built
// from it, every govulncheck run, and every cold compile. `go list` is exact, always
// present, and costs nothing.
func GoList(t *testing.T, args ...string) []string {
	t.Helper()

	// CommandContext, so a hung toolchain invocation is killed when the test's deadline
	// expires rather than blocking the package until the whole run times out.
	cmd := exec.CommandContext(t.Context(), "go", append([]string{"list"}, args...)...)
	cmd.Dir = RepoRoot(t)

	out, err := cmd.Output()
	if err != nil {
		var stderr string
		// errors.As, not a type assertion: exec wraps its error in some paths, and a bare
		// assertion silently yields no stderr exactly when the failure needs explaining.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		t.Fatalf("go list %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	return nonEmptyLines(string(out))
}

// TrackedFiles returns every file git knows about, as repo-relative slash-separated paths.
//
// Using git rather than filepath.WalkDir matters: WalkDir would also see build output,
// editor droppings, and anything else .gitignore exists to hide, so a hygiene assertion
// would fail for files that are not part of the repository at all.
func TrackedFiles(t *testing.T) []string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", "ls-files")
	cmd.Dir = RepoRoot(t)

	out, err := cmd.Output()
	if err != nil {
		t.Skipf("git ls-files failed (not a git checkout?): %v", err)
	}
	return nonEmptyLines(string(out))
}

// RunCommand execs a command from the repo root and returns its combined output.
func RunCommand(ctx context.Context, t *testing.T, name string, args ...string) (string, error) {
	t.Helper()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = RepoRoot(t)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
