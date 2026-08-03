//go:build rename

// Package main's end-to-end proof that this template is genuinely forkable.
//
// It copies the whole repository, renames it, and then BUILDS AND TESTS the result. Nothing
// short of that proves the claim: the module path reaches ninety files, two of them not Go,
// and the generated protobuf code has to be rebuilt rather than edited -- a failure in any one
// of those produces a fork that does not work, and the most dangerous failure compiles.
//
// Behind a build tag because of what it costs: a full codegen run plus a build and test pass
// over a tree whose import paths are all new, so nothing in the build cache applies to the
// repo's own packages. Measured at 25.8s with a warm module cache on this machine, and
// minutes without one. `task verify:rename` runs it.
//
// It tests what is COMMITTED, not the working tree: copyRepo lists files with `git ls-files`,
// which is also what the tool itself walks. That is the right subject -- a fork clones a
// commit -- but it means an uncommitted change to cmd/rename is not what this exercises.
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	oldModule = "github.com/example/gomicro"
	newModule = "github.com/acme/orders"
)

// TestTheRenamedRepositoryBuildsAndPasses is the whole claim, executed.
func TestTheRenamedRepositoryBuildsAndPasses(t *testing.T) {
	fork := copyRepo(t)

	// The tool requires a clean tree, so the copy has to be a real repository.
	git(t, fork, "init", "--quiet")
	git(t, fork, "config", "user.email", "rename-test@example.com")
	git(t, fork, "config", "user.name", "rename test")
	git(t, fork, "add", "-A")
	git(t, fork, "commit", "--quiet", "-m", "template")

	out := runCmd(t, fork, 10*time.Minute, "go", "run", "./cmd/rename", "-module", newModule)
	t.Logf("rename output:\n%s", out)

	// 1. The tool removed itself. A fork should not carry a one-shot tool already used.
	if _, err := os.Stat(filepath.Join(fork, "cmd", "rename")); !os.IsNotExist(err) {
		t.Error("cmd/rename still exists after a successful rename")
	}

	// 2. Nothing anywhere still names the template.
	if residual := grepTree(t, fork, oldModule); len(residual) > 0 {
		t.Errorf("%d files still reference %s:\n  %s",
			len(residual), oldModule, strings.Join(residual, "\n  "))
	}

	// 3. go.mod says what it should.
	gomod, err := os.ReadFile(filepath.Join(fork, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	if !strings.HasPrefix(string(gomod), "module "+newModule+"\n") {
		t.Errorf("go.mod does not start with the new module line:\n%s", firstLine(string(gomod)))
	}

	// 4. IT BUILDS. Necessary, and demonstrably not sufficient -- see below.
	runCmd(t, fork, 10*time.Minute, "go", "build", "./...")

	// 5. IT RUNS. This is the step that catches a textually rewritten descriptor, which
	//    compiles perfectly and then panics inside init() before main is reached:
	//
	//      panic: runtime error: slice bounds out of range [-4:]
	//        filedesc.(*File).unmarshalSeed
	//
	//    A rename test that stopped at `go build` would have shipped that.
	runCmd(t, fork, 20*time.Minute, "go", "test", "./...")
}

// copyRepo copies every tracked file into a temporary directory.
func copyRepo(t *testing.T) string {
	t.Helper()

	root := runCmd(t, "", time.Minute, "git", "rev-parse", "--show-toplevel")
	root = strings.TrimSpace(root)

	dst := t.TempDir()

	listed := runCmd(t, root, time.Minute, "git", "ls-files")
	for _, rel := range strings.Split(listed, "\n") {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		src := filepath.Join(root, filepath.FromSlash(rel))
		out := filepath.Join(dst, filepath.FromSlash(rel))

		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if err := os.WriteFile(out, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dst
}

// grepTree returns every file still containing needle.
func grepTree(t *testing.T, root, needle string) []string {
	t.Helper()

	// BOTH ERRORS ARE SWALLOWED ON PURPOSE, which is what nilerr flags below.
	//
	// This walk answers "does the renamed tree still mention the old module path anywhere?".
	// A file that cannot be walked or read cannot contain the needle, so skipping it is the
	// correct answer to the question asked -- and aborting the walk would turn a transient
	// permission blip on some unrelated file into a failure of the rename test.
	//
	// The risk this accepts is a file that DOES contain the needle being skipped because it
	// was unreadable, which would make this report a false clean. That is bounded: the tree
	// was created by this test moments earlier, in a directory it owns.
	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unwalkable entry cannot contain the needle
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil //nolint:nilerr // an unreadable file cannot contain the needle
		}
		if strings.Contains(string(data), needle) {
			rel, _ := filepath.Rel(root, path)
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return found
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return runCmd(t, dir, time.Minute, "git", args...)
}

func runCmd(t *testing.T, dir string, timeout time.Duration, name string, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
