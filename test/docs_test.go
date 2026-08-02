package test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/testutil"
)

// DELETING.md IS INSTRUCTIONS PEOPLE FOLLOW LITERALLY, which makes it the documentation with
// the shortest path from "slightly stale" to "broke someone's repository".
//
// It names about seventy paths. Every refactor that moves one of them silently invalidates a
// step, and the reader who discovers it has already deleted something else. Nothing compiles a
// markdown file, so this does.

// docPath matches a backticked path that looks like a repository path: it contains a slash or
// ends in a known extension, and starts with something plausible.
//
// Prose is full of backticked things that are not paths -- `AUTH_MODE=dev`, `codes.Unknown`,
// `go mod tidy` -- so the pattern is deliberately narrow and the checks below narrow it
// further. A guard that false-positives on prose is a guard people delete.
var docPath = regexp.MustCompile("`([a-zA-Z0-9_./-]+)`")

// TestDeletingGuideNamesRealPaths keeps every instruction actionable.
func TestDeletingGuideNamesRealPaths(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)

	raw, err := os.ReadFile(filepath.Join(root, "docs", "DELETING.md"))
	if err != nil {
		t.Fatalf("read docs/DELETING.md: %v", err)
	}

	// Directories that the guide names as things to delete legitimately include ones that a
	// PREVIOUS step already removed in the reader's tree -- but in THIS tree they must all
	// still exist, because nothing has been deleted here.
	var checked int
	for _, m := range docPath.FindAllStringSubmatch(string(raw), -1) {
		candidate := m[1]
		if !looksLikeRepoPath(candidate) {
			continue
		}

		// A trailing slash means a directory; a glob means a set. Reduce both to something
		// checkable: the directory that contains them.
		target := strings.TrimSuffix(candidate, "/")
		if i := strings.Index(target, "*"); i >= 0 {
			target = filepath.Dir(target[:i])
		}
		if target == "" || target == "." {
			continue
		}

		checked++
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(target))); err != nil {
			t.Errorf("docs/DELETING.md names %q, which does not exist.\n\n"+
				"Someone following that step deletes the wrong thing, or nothing, and finds out "+
				"after they have already removed something else. Update the guide with the move.",
				candidate)
		}
	}

	if checked < 30 {
		t.Fatalf("only %d paths were checked; the guide names dozens, so this pattern has "+
			"stopped matching and the guard is passing vacuously", checked)
	}
}

// looksLikeRepoPath filters prose out of the backtick matches.
func looksLikeRepoPath(s string) bool {
	// Environment variables, Go identifiers and shell fragments all arrive here.
	if strings.ContainsAny(s, "=") || strings.HasPrefix(s, ".") {
		return false
	}

	knownRoots := []string{
		"cmd/", "internal/", "test/", "deploy/", "proto/", "gen/", "docs/", "tools/",
	}
	for _, prefix := range knownRoots {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}

	// Top-level files the guide edits.
	switch s {
	case "go.mod", "buf.gen.yaml", "Taskfile.yml", "README.md":
		return true
	}
	return false
}

// TestDeletingGuideCoversEveryOptionalPackage stops a new subsystem shipping undeletable.
//
// The guide's value is that it is COMPLETE: a reader trusts that anything not listed is load
// bearing. A subsystem added later and never written up quietly breaks that promise, and the
// only signal is a reader wondering why their fork still depends on something.
func TestDeletingGuideCoversEveryOptionalPackage(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)

	raw, err := os.ReadFile(filepath.Join(root, "docs", "DELETING.md"))
	if err != nil {
		t.Fatalf("read docs/DELETING.md: %v", err)
	}
	guide := string(raw)

	// The packages the guide must have an opinion about -- either a removal recipe, or a place
	// in the "cannot be removed" list.
	for _, pkg := range []string{
		"internal/platform/client",
		"internal/platform/ratelimit",
		"internal/platform/events",
		"internal/platform/outbox",
		"internal/platform/gormx",
		"internal/platform/migrations",
		"internal/platform/testdb",
		"internal/platform/observability",
		"internal/platform/auth",
		"internal/platform/apperr",
		"internal/platform/config",
		"internal/platform/interceptor",
		"internal/gateway",
		"internal/order/orderpg",
		"internal/order/orderproj",
		"cmd/worker",
		"cmd/migrate",
		"deploy",
	} {
		if !strings.Contains(guide, pkg) {
			t.Errorf("docs/DELETING.md never mentions %s.\n\n"+
				"Either write its removal recipe, or add it to \"What cannot be removed\" with "+
				"the reason. Silence reads as \"nobody thought about it\", which is the one thing "+
				"a guide like this must not say.", pkg)
		}
	}
}

// mdLink matches a markdown link target.
var mdLink = regexp.MustCompile(`\]\(([^)"'#\s]+)`)

// TestAdrIndexLinksResolve keeps the decision index honest.
//
// docs/adr/README.md is an index rather than a set of documents, which is only defensible while
// every pointer lands somewhere. A stale link is worse here than in ordinary prose: the whole
// argument for not writing eighteen ADRs is that the reasoning is findable where it lives, and a
// broken link is that claim failing.
func TestAdrIndexLinksResolve(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)
	adrDir := filepath.Join(root, "docs", "adr")

	raw, err := os.ReadFile(filepath.Join(adrDir, "README.md"))
	if err != nil {
		t.Fatalf("read docs/adr/README.md: %v", err)
	}

	var checked int
	for _, m := range mdLink.FindAllStringSubmatch(string(raw), -1) {
		target := m[1]
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
			continue
		}
		checked++

		resolved := filepath.Join(adrDir, filepath.FromSlash(target))
		if _, err := os.Stat(resolved); err != nil {
			t.Errorf("docs/adr/README.md links to %q, which does not exist.\n\n"+
				"This index is the whole justification for not writing a document per decision: "+
				"the reasoning is findable where it lives. A link that lands nowhere is that "+
				"argument failing.", target)
		}
	}

	if checked < 20 {
		t.Fatalf("only %d links were checked; the index carries dozens, so the pattern has "+
			"stopped matching", checked)
	}
}

// TestEveryAdrIsListedInTheIndex catches the opposite drift: a document nobody links to.
func TestEveryAdrIsListedInTheIndex(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)
	adrDir := filepath.Join(root, "docs", "adr")

	raw, err := os.ReadFile(filepath.Join(adrDir, "README.md"))
	if err != nil {
		t.Fatalf("read docs/adr/README.md: %v", err)
	}
	index := string(raw)

	entries, err := os.ReadDir(adrDir)
	if err != nil {
		t.Fatalf("read docs/adr: %v", err)
	}

	var found int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == "README.md" || filepath.Ext(name) != ".md" {
			continue
		}
		found++
		if !strings.Contains(index, name) {
			t.Errorf("docs/adr/%s exists but the index does not list it.\n\n"+
				"An unlisted decision document is one nobody finds, which is the failure mode "+
				"this repository chose an index to avoid.", name)
		}
	}

	if found == 0 {
		t.Fatal("no ADRs were found, so this guard passes vacuously")
	}
}
