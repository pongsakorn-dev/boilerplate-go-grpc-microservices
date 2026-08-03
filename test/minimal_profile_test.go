//go:build profile

package test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestDeletingTheOptionalSubsystemsLeavesAWorkingRepo executes docs/DELETING.md.
//
// # What this proves, and what it deliberately does not
//
// The guide is instructions people follow literally, and its most expensive failure is not a
// stale path -- docs_test.go's TestDeletingGuideNamesRealPaths already catches those. It is
// INCOMPLETENESS: you delete what a section lists, run `go build`, and discover three more
// files that referenced it and which the section never mentioned. At that point you are
// debugging someone else's architecture with half of it already gone.
//
// So this walks the guide top to bottom, in its own documented order, deleting each section's
// files cumulatively and building after each one -- which is literally the instruction the
// guide gives ("After each section: go build ./... && go test ./..."). Every file that fails to
// compile must be a file some already-processed section NAMES. A build error in a file the
// guide has not mentioned is the failure this test exists to find.
//
// IT DOES NOT APPLY THE WIRING EDITS, and cannot. Those are prose -- "remove buildGateway and
// its call", "remove the oidc arm of NewVerifier" -- and encoding them in Go would mean this
// test executed a Go translation of the guide rather than the guide itself, proving the
// translation correct while the prose rotted. So the build is EXPECTED to fail as sections
// accumulate; what is asserted is that it only ever fails somewhere the guide already told you
// to go.
//
// Behind //go:build profile because it copies the repository and builds it eleven times.
func TestDeletingTheOptionalSubsystemsLeavesAWorkingRepo(t *testing.T) {
	root := repoRoot(t)
	sections := parseDeletingGuide(t, root)

	if len(sections) < 8 {
		t.Fatalf("parsed only %d sections out of docs/DELETING.md; the guide's shape has "+
			"changed and this test is no longer reading it", len(sections))
	}

	work := copyTrackedFiles(t, root)

	// The repo must build BEFORE anything is deleted, or every assertion below is measuring
	// a pre-existing failure.
	if out, ok := goBuild(t, work); !ok {
		t.Fatalf("the pristine copy does not build, so nothing below means anything:\n%s", out)
	}

	// explained accumulates every path named by every section processed so far. A file may
	// legitimately break in section 5 and stay broken through section 10.
	explained := map[string]bool{}

	for _, s := range sections {
		t.Run(s.slug(), func(t *testing.T) {
			for p := range s.names {
				explained[p] = true
			}

			removed := deletePaths(t, work, s.deletes)
			if len(removed) == 0 && len(s.deletes) > 0 {
				t.Fatalf("section %q named %d paths to delete and none existed in the copy",
					s.title, len(s.deletes))
			}

			out, ok := goBuild(t, work)
			if ok {
				// Nothing broken: the section's remaining bullets are cleanup, not repair.
				t.Logf("removed %d paths; the repo still builds", len(removed))
				return
			}

			broken := erroringFiles(out)

			// Logged on every run, because "PASS" here is ambiguous otherwise -- it means
			// either "the build survived" or "it broke exactly where the guide said". Those
			// are very different states and a reader deserves to know which one they got.
			t.Logf("removed %d paths; %d file(s) now need the edits this section describes: %s",
				len(removed), len(broken), strings.Join(broken, ", "))

			var unexplained []string
			for _, file := range broken {
				if !isExplained(file, explained) {
					unexplained = append(unexplained, file)
				}
			}
			if len(unexplained) > 0 {
				sort.Strings(unexplained)
				t.Errorf("after deleting section %q the build fails in files the guide never "+
					"mentions:\n\n  %s\n\nSomeone following these instructions reaches this "+
					"point with the subsystem already gone and no idea where to go next. Add "+
					"each file to that section's Wiring or Config bullet.\n\nbuild output:\n%s",
					s.title, strings.Join(unexplained, "\n  "), tailLines(out, 25))
			}
		})
	}
}

// section is one "## N. Title" block of the guide.
type section struct {
	title string

	// deletes are the paths its Delete and Tests bullets name -- what a reader removes.
	deletes []string

	// names is every repo path the section mentions ANYWHERE, which is the set a resulting
	// build error is allowed to land in.
	names map[string]bool
}

func (s section) slug() string {
	slug := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return '_'
		}
	}, s.title)
	return strings.Trim(slug, "_")
}

var (
	sectionRE = regexp.MustCompile(`(?m)^## (\d+)\. (.+)$`)
	backtick  = regexp.MustCompile("`([^`]+)`")
	// A bullet whose label marks files the reader removes rather than edits.
	deleteBulletRE = regexp.MustCompile(`(?m)^- \*\*(Delete|Tests deleted with it):\*\*`)
	anyBulletRE    = regexp.MustCompile(`(?m)^- \*\*`)
)

// parseDeletingGuide reads the guide into sections.
//
// Structure-driven rather than a hardcoded list: a section added to the guide and not to this
// test would otherwise be a section nobody executes, which is the same rot in a new place.
func parseDeletingGuide(t *testing.T, root string) []section {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(root, "docs", "DELETING.md"))
	if err != nil {
		t.Fatalf("read docs/DELETING.md: %v", err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")

	heads := sectionRE.FindAllStringSubmatchIndex(text, -1)
	var out []section

	for i, h := range heads {
		end := len(text)
		if i+1 < len(heads) {
			end = heads[i+1][0]
		}
		body := text[h[1]:end]

		s := section{title: text[h[4]:h[5]], names: map[string]bool{}}

		for _, tok := range backtick.FindAllStringSubmatch(body, -1) {
			if p := repoPath(tok[1]); p != "" {
				s.names[p] = true
			}
		}
		for _, bullet := range deleteBullets(body) {
			for _, tok := range backtick.FindAllStringSubmatch(bullet, -1) {
				if p := repoPath(tok[1]); p != "" {
					s.deletes = append(s.deletes, p)
				}
			}
		}

		out = append(out, s)
	}
	return out
}

// deleteBullets returns the text of the bullets that name things to remove.
func deleteBullets(body string) []string {
	var out []string

	starts := deleteBulletRE.FindAllStringIndex(body, -1)
	for _, s := range starts {
		rest := body[s[0]+2:] // past the "- "
		if next := anyBulletRE.FindStringIndex(rest); next != nil {
			rest = rest[:next[0]]
		}
		out = append(out, rest)
	}
	return out
}

// repoPath decides whether a backticked token is a path in this repository.
//
// Conservative on purpose. The guide backticks identifiers (`UpstreamConfig`), environment
// variables (`NATS_URL=""`) and shell (`go mod tidy`) as well as paths, and treating one of
// those as a file to delete would make this test destroy something arbitrary.
func repoPath(tok string) string {
	tok = strings.TrimSpace(tok)
	if tok == "" || strings.ContainsAny(tok, " =\"'()") {
		return ""
	}
	hasDir := strings.Contains(tok, "/")
	hasExt := regexp.MustCompile(`\.(go|ya?ml|sql|proto|md|json)$`).MatchString(tok)
	if !hasDir && !hasExt {
		return ""
	}
	// Only inside the repo's own trees.
	for _, top := range []string{"cmd/", "internal/", "test/", "deploy/", "proto/", "gen/", "docs/"} {
		if strings.HasPrefix(tok, top) {
			return tok
		}
	}
	// Root-level files the guide names, e.g. buf.gen.yaml or go.mod.
	if !hasDir && hasExt {
		return tok
	}
	return ""
}

// deletePaths removes what a section lists, expanding globs, and reports what it removed.
func deletePaths(t *testing.T, work string, paths []string) []string {
	t.Helper()

	var removed []string
	for _, p := range paths {
		full := filepath.Join(work, filepath.FromSlash(p))

		if strings.ContainsAny(p, "*?") {
			matches, err := filepath.Glob(full)
			if err != nil {
				t.Fatalf("glob %s: %v", p, err)
			}
			for _, m := range matches {
				if err := os.RemoveAll(m); err != nil {
					t.Fatalf("remove %s: %v", m, err)
				}
				removed = append(removed, m)
			}
			continue
		}

		if _, err := os.Stat(full); err != nil {
			continue // docs_test.go owns "does this path exist"; not this test's job
		}
		if err := os.RemoveAll(full); err != nil {
			t.Fatalf("remove %s: %v", p, err)
		}
		removed = append(removed, p)
	}
	return removed
}

// isExplained reports whether a failing file is covered by something the guide named.
//
// Prefix-aware, because a section that says `internal/app/app.go` explains an error in that
// file, and one that says `internal/gateway/` explains errors anywhere beneath it.
func isExplained(file string, explained map[string]bool) bool {
	file = filepath.ToSlash(file)
	for named := range explained {
		named = strings.TrimSuffix(named, "/")
		if file == named || strings.HasPrefix(file, named+"/") || strings.HasPrefix(file, named) {
			return true
		}
	}
	return false
}

// goBuild builds the copy and reports the combined output.
func goBuild(t *testing.T, dir string) (string, bool) {
	t.Helper()

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir

	// GOPROXY=off, and it is not an optimisation.
	//
	// Deleting a package leaves its importers referring to an import path that no longer
	// resolves locally, and the Go tool's next move is to assume it is a MODULE it has not
	// downloaded -- so it runs `git ls-remote` against github.com/example/gomicro and waits.
	// Observed while sabotaging this test: a build error arrived wrapped in a VCS failure,
	// several seconds late, and only because the machine had network.
	//
	// Turning the proxy off makes the same failure immediate, deterministic and offline, which
	// this repository promises everywhere else. The error still names the file, which is all
	// this test reads.
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")

	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// erroringFiles pulls repo-relative file paths out of `go build` output.
var buildErrorRE = regexp.MustCompile(`(?m)^([^\s:]+\.go):\d+`)

func erroringFiles(out string) []string {
	seen := map[string]bool{}
	var files []string

	for _, m := range buildErrorRE.FindAllStringSubmatch(out, -1) {
		f := filepath.ToSlash(m[1])
		if !seen[f] {
			seen[f] = true
			files = append(files, f)
		}
	}
	return files
}

// copyTrackedFiles copies exactly what git tracks into a scratch directory.
//
// git ls-files rather than a filesystem walk: it skips .git, build caches and anything
// gitignored, and it is the same definition of "the repository" that cmd/rename's end-to-end
// test uses.
func copyTrackedFiles(t *testing.T, root string) string {
	t.Helper()

	dst := t.TempDir()

	listed := run(t, root, time.Minute, "git", "ls-files")
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
			continue // a tracked file deleted in the working tree is not this test's problem
		}
		if err := os.WriteFile(out, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dst
}

func repoRoot(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(run(t, "", time.Minute, "git", "rev-parse", "--show-toplevel"))
}

func run(t *testing.T, dir string, _ time.Duration, name string, args ...string) string {
	t.Helper()

	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
