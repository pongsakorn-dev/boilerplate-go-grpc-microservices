package test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/testutil"
)

// Repository hygiene guards.
//
// Every assertion here corresponds to a way this template can break on a machine that is
// not the author's -- most of them specific to Windows, all of them producing failures
// that a newcomer cannot diagnose from the error message.

// TestNoCRLFInTrackedFiles is the guard that protects every other guard.
//
// On Windows, core.autocrlf=true is a common default. Without .gitattributes, git checks
// out CRLF while every code generator (buf, protoc-gen-go, gofmt) writes LF. That silently
// breaks the committed-codegen diff test, every golden-file comparison, and gofmt in CI --
// and the symptom is "the template's own tests fail on a fresh clone", which is the single
// most adoption-fatal thing that can happen.
//
// .gitattributes prevents it. This test proves .gitattributes is still doing its job.
func TestNoCRLFInTrackedFiles(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)
	var offenders []string

	for _, rel := range testutil.TrackedFiles(t) {
		if isBinaryPath(rel) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			// A tracked-but-absent file is a different problem; skip rather than
			// mis-report it as a line-ending failure.
			continue
		}
		if bytes.Contains(b, []byte("\r\n")) {
			offenders = append(offenders, rel)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("these tracked files contain CRLF line endings:\n  %s\n\n"+
			"Fix: confirm .gitattributes contains `* text=auto eol=lf`, then re-normalize with\n"+
			"  git add --renormalize .",
			strings.Join(offenders, "\n  "))
	}
}

// TestNoSymlinks guards against a Windows checkout silently corrupting the tree.
//
// core.symlinks defaults to false on Windows (creating one requires Developer Mode or
// elevation). A symlink committed on Linux checks out on Windows as a PLAIN TEXT FILE whose
// contents are the target path. Nothing errors; you just get a file with mysterious
// contents, and any test reading it fails for an unrelated-looking reason.
func TestNoSymlinks(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("git", "ls-files", "-s")
	cmd.Dir = testutil.RepoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("git ls-files -s failed: %v", err)
	}

	var offenders []string
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		// Format: <mode> <object> <stage>\t<path>. Mode 120000 is a symlink.
		if strings.HasPrefix(line, "120000 ") {
			if _, path, ok := strings.Cut(line, "\t"); ok {
				offenders = append(offenders, path)
			}
		}
	}

	if len(offenders) > 0 {
		t.Errorf("symlinks are tracked, which check out as plain text files on Windows:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestNoShellScripts keeps the repository free of .sh files.
//
// Two independent reasons:
//   - The production image is distroless. It contains no shell at all, so an entrypoint
//     script cannot run -- the container would exit with a confusing exec error.
//   - A .sh committed with CRLF fails on Linux with `bad interpreter: /bin/sh^M`.
//
// Anything script-shaped belongs in cmd/devtool, in Go, where it is genuinely portable.
func TestNoShellScripts(t *testing.T) {
	t.Parallel()

	var offenders []string
	for _, rel := range testutil.TrackedFiles(t) {
		switch strings.ToLower(filepath.Ext(rel)) {
		case ".sh", ".bash", ".ps1", ".cmd", ".bat":
			offenders = append(offenders, rel)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("shell scripts are tracked:\n  %s\n\n"+
			"The production image is distroless and has no shell. Put this logic in "+
			"cmd/devtool instead, where it runs on every platform.",
			strings.Join(offenders, "\n  "))
	}
}

// TestNoLongPaths keeps every tracked path short enough to survive a Windows checkout.
//
// Windows caps a path at 260 characters unless both core.longpaths and the OS policy are
// enabled -- and a cloner will not have configured either. The repository may also be
// cloned into an already-deep directory, so the budget here is deliberately conservative:
// 150 characters of repo-relative path leaves ~110 for wherever it is cloned.
func TestNoLongPaths(t *testing.T) {
	t.Parallel()

	const maxLen = 150

	var offenders []string
	for _, rel := range testutil.TrackedFiles(t) {
		if len(rel) > maxLen {
			offenders = append(offenders, rel)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("these tracked paths exceed %d characters and risk breaking a Windows clone:\n  %s",
			maxLen, strings.Join(offenders, "\n  "))
	}
}

// TestGitattributesIsPresentAndCovering asserts the file every other guard depends on both
// exists and still contains the catch-all rule.
func TestGitattributesIsPresentAndCovering(t *testing.T) {
	t.Parallel()

	b, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), ".gitattributes"))
	if err != nil {
		t.Fatalf(".gitattributes is missing: %v\n\n"+
			"It must be present, and it must be committed BEFORE any generated file.", err)
	}

	if !strings.Contains(string(b), "* text=auto eol=lf") {
		t.Error(".gitattributes no longer contains `* text=auto eol=lf`; " +
			"without the catch-all, any newly added file type reverts to the platform default")
	}
}

// isBinaryPath reports whether a path is expected to contain non-text bytes.
func isBinaryPath(rel string) bool {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".ico", ".pdf",
		".zip", ".gz", ".binpb", ".pb", ".golden":
		return true
	}
	return false
}
