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

	// M\d+[a-z]? -- the optional letter matters. A bare M\d+ matches "M8" inside BOTH "M8a"
	// and "M8b", so finishing M8a would flag M8b as stale even though it is genuinely still
	// pending. Found the first time a milestone was split in two.
	milestone := regexp.MustCompile(`M\d+[a-z]?`) // for the Done line
	done := map[string]bool{}
	for _, m := range milestone.FindAllString(doneLine, -1) {
		done[m] = true
	}
	if len(done) == 0 {
		t.Fatal("the `**Done:**` line names no milestones, so this guard is vacuous")
	}

	// Only the token IMMEDIATELY AFTER the marker counts.
	//
	// Scanning the whole line was wrong twice. A row's NOTES legitimately mention other
	// milestones -- "⬜ M8b | Deliberately after M10, so the deploy story exists first" -- and
	// reading M10 out of that prose flagged a genuinely pending row as stale the moment M10
	// shipped. The marker is the claim; the rest of the line is commentary.
	marked := regexp.MustCompile(`⬜\s*(M\d+[a-z]?)`)

	for _, line := range strings.Split(content, "\n") {
		m := marked.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if done[m[1]] {
			t.Errorf("%s is listed as done in the roadmap but still marked ⬜ here:\n  %s",
				m[1], strings.TrimSpace(line))
		}
	}
}

// diagramPath matches a repo-relative package path inside a diagram node label. The trailing
// /* form is allowed and checked as the directory without it.
var diagramPath = regexp.MustCompile(`\b(?:cmd|internal|gen|proto|deploy|test|tools)/[A-Za-z0-9_./*-]*[A-Za-z0-9_*]`)

// TestDiagramsNameRealPackages closes the direction TestReadmeDocumentsEveryPackage
// deliberately leaves open.
//
// That guard is forward-only: every real directory must appear in the layout tree. The
// reverse -- documentation naming a package that does NOT exist -- was left out on purpose,
// because prose contains slashes and parsing it strictly produces false positives that teach
// people to disable the test. That reasoning is sound for prose and wrong for diagrams.
//
// A mermaid node label is not prose. It is a short, structured string, and checking it costs
// nothing. Leaving it unchecked let the architecture diagram -- the first structural picture
// anyone sees, at the top of the Architecture section -- spend the repository's whole life
// naming the Postgres adapter "gormstore", a package that has never existed under any name
// in any commit. The real one is internal/order/orderpg, it shipped, and it has integration
// tests. The diagram also styled it as pending work.
//
// The failure mode is specific and bad: a diagram is what a reader trusts BEFORE they know
// enough to check it, so it is the worst place in the document to be wrong and the least
// likely place for a reader to notice.
func TestDiagramsNameRealPackages(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)

	var checked int
	for _, rel := range testutil.TrackedFiles(t) {
		if filepath.Ext(rel) != ".md" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}

		inDiagram := false
		for i, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inDiagram = strings.HasPrefix(strings.TrimSpace(line), "```mermaid")
				continue
			}
			if !inDiagram {
				continue
			}
			for _, cited := range diagramPath.FindAllString(line, -1) {
				checked++
				// internal/platform/* means "the packages under it", so check the parent.
				dir := strings.TrimSuffix(strings.TrimSuffix(cited, "*"), "/")
				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir))); err == nil {
					continue
				}
				t.Errorf("%s:%d diagram names %q, which is not a directory in this repo",
					rel, i+1, cited)
			}
		}
	}

	if checked == 0 {
		t.Fatal("found no package paths in any diagram -- this guard would silently pass forever")
	}
}

// delimiterRow matches a GitHub-flavoured-markdown table delimiter: |---|---| and its
// alignment variants |:--|--:|:-:|.
var delimiterRow = regexp.MustCompile(`^\|[\s:|-]*-[\s:|-]*\|$`)

// TestMarkdownTablesAreWellFormed catches the defect that makes a section unreadable in the
// one place people actually read it.
//
// A GFM table is a header row, a delimiter row, then body rows. Break the delimiter and the
// rows do not become an ugly table -- they stop being a table at all and render as one
// paragraph of literal pipe characters. There is no warning: the file is valid markdown, and
// in a plain-text editor it still lines up perfectly.
//
// That is exactly what happened to the configuration reference. A paragraph was inserted mid
// table, terminating it, and the nine AUTH_MODE and OIDC_* rows after it became prose --
// which is to say the entire authentication configuration reference was unreadable on GitHub,
// while looking completely fine to everyone editing it locally.
//
// It is worth a guard rather than a fix because the failure is invisible to the author, the
// reviewer, and every other test in this repository. The rule is mechanical, so a machine
// should be the one checking it.
func TestMarkdownTablesAreWellFormed(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)

	var checked int
	for _, rel := range testutil.TrackedFiles(t) {
		if filepath.Ext(rel) != ".md" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}

		lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")

		// Fenced code blocks are skipped: a shell transcript or an ASCII diagram may
		// legitimately begin a line with a pipe, and it is not a table.
		inFence := false

		for i := 0; i < len(lines); i++ {
			line := strings.TrimSpace(lines[i])
			if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
				inFence = !inFence
				continue
			}
			if inFence || !strings.HasPrefix(line, "|") {
				continue
			}

			// The start of a run of table-shaped lines. Find where it ends.
			start := i
			for i+1 < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i+1]), "|") {
				i++
			}
			run := lines[start : i+1]
			checked++

			// A lone table-shaped line is not a table either -- GFM needs at least a header
			// and a delimiter -- so it renders as literal pipes just the same.
			if len(run) < 2 {
				t.Errorf("%s:%d is a table row on its own, so it renders as literal text:\n  %s",
					rel, start+1, strings.TrimSpace(run[0]))
				continue
			}
			if !delimiterRow.MatchString(strings.TrimSpace(run[1])) {
				t.Errorf("%s:%d starts a table whose second line is not a |---|---| delimiter:\n"+
					"  %s\n  %s\n\n"+
					"Without it GitHub renders every row below as one paragraph of pipe\n"+
					"characters. The usual cause is a paragraph inserted into the middle of a\n"+
					"longer table, which silently splits it in two and leaves the second half\n"+
					"with no header.",
					rel, start+1, strings.TrimSpace(run[0]), strings.TrimSpace(run[1]))
			}
		}
	}

	if checked == 0 {
		t.Fatal("found no markdown tables at all -- this guard would silently pass forever")
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
