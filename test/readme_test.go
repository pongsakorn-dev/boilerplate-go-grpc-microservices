package test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/testutil"
)

// THE README IS PART OF THE PRODUCT, and these guards treat it that way.
//
// This template's opening claim is "what is here works and is tested; what is not here is
// listed as not here". A README that has drifted from the tree is that claim failing on its
// own front page -- and it is the single most likely thing to rot, because nothing compiles
// it and every milestone is tempting to finish without it.
//
// Two milestones in a row (auth, then Postgres) shipped with a repository-layout section that
// omitted every package they added: orderpg, gormx, migrations, testdb, testjwks, cmd/migrate
// and deploy/keycloak all existed on disk and nowhere in the docs. Nothing caught it because
// nothing was looking. Now something is.

// TestReadmeDocumentsEveryPackage is the guard that would have caught that drift.
//
// Forward direction only -- every real directory must appear in the layout tree. The reverse
// (the tree naming something deleted) is left to the link and citation guards, because prose
// legitimately contains slashes and parsing it strictly produces false positives that teach
// people to disable the test.
func TestReadmeDocumentsEveryPackage(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)
	layout := readmeSection(t, root, "## Repository layout")

	var checked int
	for _, top := range []string{"cmd", "internal", "deploy"} {
		base := filepath.Join(root, top)
		if _, err := os.Stat(base); err != nil {
			continue
		}

		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return err
			}
			name := d.Name()
			if name == "." || strings.HasPrefix(name, ".") {
				return nil
			}
			checked++

			// "name/" rather than bare "name": the trailing slash is what distinguishes a
			// directory entry in the tree from the same word appearing in a description.
			if !strings.Contains(layout, name+"/") {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s exists but is not in the README's repository layout.\n\n"+
					"A reader's first question is \"what is in here?\", and the layout tree is "+
					"where they look. A package missing from it is a package nobody finds.",
					filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}

	if checked == 0 {
		t.Fatal("found no package directories -- this guard would pass forever")
	}
}

// backtickedTest matches `TestSomething` in markdown.
//
// The backticks are the signal. test/citations_test.go deliberately ignores bare test names
// in prose because they false-positive too easily; a name the author wrapped in code
// formatting inside a table is a claim about a specific test, and is worth holding to.
var backtickedTest = regexp.MustCompile("`(Test[A-Za-z0-9_]+)`")

// TestDocsOnlyNameRealTests stops the guard tables becoming fiction.
//
// The README's guard table is its most load-bearing section: it is the evidence for every
// claim the rest of the document makes. A row naming a test that does not exist is worse than
// no row at all, because a reader checking "is that really enforced?" finds a line saying yes.
//
// This is the same failure citations_test.go catches in source comments; the README simply
// has more surface for it.
func TestDocsOnlyNameRealTests(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)

	declared := map[string]bool{}
	declRE := regexp.MustCompile(`^func (Test[A-Za-z0-9_]+)\(`)
	for _, rel := range testutil.TrackedFiles(t) {
		if !strings.HasSuffix(rel, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if m := declRE.FindStringSubmatch(line); m != nil {
				declared[m[1]] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no test functions -- this guard would pass forever")
	}

	var namedAnywhere int
	for _, rel := range testutil.TrackedFiles(t) {
		if filepath.Ext(rel) != ".md" || strings.HasPrefix(rel, "proto/third_party/") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		for _, m := range backtickedTest.FindAllStringSubmatch(string(b), -1) {
			namedAnywhere++
			if !declared[m[1]] {
				t.Errorf("%s names `%s`, which is not a test function anywhere in the repo.\n\n"+
					"Either write it, rename the reference, or drop the backticks if it is an "+
					"illustration rather than a claim.", filepath.ToSlash(rel), m[1])
			}
		}
	}

	if namedAnywhere == 0 {
		t.Fatal("no documentation names any test -- either the guard tables vanished or this " +
			"guard has stopped matching, and both are worth knowing")
	}
}

// TestReadmeStatusTableHasNoStaleMilestones catches the smallest, most common drift: a
// milestone marked as upcoming after it has shipped.
//
// It reads the roadmap's own "Done:" line as the source of truth and checks no milestone
// listed there is still carrying a ⬜ marker in the status table above.
func TestReadmeStatusTableHasNoStaleMilestones(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	content := string(b)

	// Read the whole PARAGRAPH, not the first line.
	//
	// Markdown wraps, and this repo wraps at 90 columns, so the Done list spans two lines.
	// The first version of this guard read one line and therefore knew only about M0-M3 --
	// it was blind to exactly the two milestones most recently finished, which are the ones
	// a staleness check exists to catch. Found by sabotage: marking a done milestone as ⬜
	// left the guard green.
	lines := strings.Split(content, "\n")
	doneLine := ""
	for i, line := range lines {
		if !strings.HasPrefix(line, "**Done:**") {
			continue
		}
		var b strings.Builder
		for _, l := range lines[i:] {
			if strings.TrimSpace(l) == "" {
				break
			}
			b.WriteString(l + " ")
		}
		doneLine = b.String()
		break
	}
	if doneLine == "" {
		t.Skip("the roadmap has no `**Done:**` line to compare against")
	}

	milestone := regexp.MustCompile(`M\d+`)
	done := map[string]bool{}
	for _, m := range milestone.FindAllString(doneLine, -1) {
		done[m] = true
	}
	if len(done) == 0 {
		t.Fatal("the `**Done:**` line names no milestones, so this guard is vacuous")
	}

	for _, line := range strings.Split(content, "\n") {
		if !strings.Contains(line, "⬜") {
			continue
		}
		for _, m := range milestone.FindAllString(line, -1) {
			if done[m] {
				t.Errorf("%s is listed as done in the roadmap but still marked ⬜ here:\n  %s",
					m, strings.TrimSpace(line))
			}
		}
	}
}

// readmeSection returns the text between a heading and the next top-level heading.
func readmeSection(t *testing.T, root, heading string) string {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	content := string(b)

	start := strings.Index(content, heading)
	if start < 0 {
		t.Fatalf("README has no %q section", heading)
	}
	rest := content[start+len(heading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		return rest[:end]
	}
	return rest
}
