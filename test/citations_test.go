package test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/testutil"
)

// selfExcluded is this file. A detector has to quote the patterns it detects -- the doc
// comments below name "file_test.go::TestName" as the citation form and cite a real
// historical drift (config_test.go::TestValidate_RejectsDevAuthOutsideDev) as the motivating
// example. Scanning itself turns both into false positives.
//
// This is the same self-reference trap that tiers_test.go hit: a text-scanning guard whose
// own rules are written in the text it scans. Excluding exactly one file, by name, keeps the
// guard's teeth everywhere else.
const selfExcluded = "test/citations_test.go"

// testFileRE matches a path ending in _test.go mentioned anywhere in prose.
var testFileRE = regexp.MustCompile(`[A-Za-z0-9_./-]*[a-z0-9_]+_test\.go`)

// TestCommentsDoNotCiteMissingTests is the guard protecting this project's central claim.
//
// The README says every architectural decision is "proven by a test rather than a claim in
// a README". Comments throughout the source cite specific test files as the proof. At the
// time this guard was written, FIFTEEN of those citations named files that did not exist --
// including one inside proto/order/v1/order.proto, which protoc copies verbatim into
// gen/go/order/v1/order.pb.go and therefore ships to every consumer of the package.
//
// That is worse than an ordinary stale comment. A reader who checks one citation, finds
// nothing, and concludes the proofs are decorative has been given a reason to distrust all
// 1,100 comment lines -- and checking is exactly what the README invites them to do.
//
// The rule this enforces: a comment may name a test file only if that file exists NOW.
// Future work is written in the future tense with its milestone, e.g.
// "M9 will assert this in test/interservice_test.go", which this guard deliberately allows
// by requiring the "will" form to be outside a bare citation.
func TestCommentsDoNotCiteMissingTests(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)

	// Every test file that actually exists, by basename and by repo-relative path.
	existing := map[string]bool{}
	for _, rel := range testutil.TrackedFiles(t) {
		if strings.HasSuffix(rel, "_test.go") {
			existing[rel] = true
			existing[filepath.ToSlash(filepath.Base(rel))] = true
		}
	}
	if len(existing) == 0 {
		t.Fatal("found no test files at all -- this guard would silently pass forever")
	}

	type violation struct{ file, cited string }
	var violations []violation

	for _, rel := range testutil.TrackedFiles(t) {
		// gen/ is excluded: protoc copies .proto comments verbatim, so a bad citation there
		// is a symptom whose cause is the .proto, which IS checked. Reporting both would
		// send someone to edit generated code.
		if strings.HasPrefix(rel, "gen/") || strings.HasPrefix(rel, "proto/third_party/") {
			continue
		}
		if rel == selfExcluded {
			continue
		}
		switch filepath.Ext(rel) {
		case ".go", ".proto", ".yml", ".yaml", ".md":
		default:
			continue
		}

		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}

		for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
			// Future-tense references are allowed: they promise rather than assert.
			if strings.Contains(line, " will ") || strings.Contains(line, "when it lands") {
				continue
			}
			for _, cited := range testFileRE.FindAllString(line, -1) {
				cited = strings.TrimPrefix(cited, "./")
				if existing[cited] || existing[filepath.ToSlash(filepath.Base(cited))] {
					continue
				}
				violations = append(violations, violation{rel, cited})
			}
		}
	}

	if len(violations) > 0 {
		sort.Slice(violations, func(i, j int) bool { return violations[i].file < violations[j].file })
		var b strings.Builder
		for _, v := range violations {
			b.WriteString("  " + v.file + " cites " + v.cited + "\n")
		}
		t.Errorf("comments cite test files that do not exist:\n%s\n"+
			"This repository's central claim is that guarantees are proven by tests rather than\n"+
			"asserted in prose. A citation to a test that does not exist is exactly the kind of\n"+
			"claim it says it refuses to make.\n\n"+
			"Fix each one by either writing the test, or rewriting the sentence in the future\n"+
			"tense with its milestone (\"M9 will assert this in ...\"), which this guard allows.",
			b.String())
	}
}

// TestCommentsDoNotCiteMissingTestFunctions catches the subtler version: the file exists,
// but the named Test function inside it does not.
//
// This is how a rename drifts. devauth.go cited
// config_test.go::TestValidate_RejectsDevAuthOutsideDev; the real function is
// TestValidateRejectsDevAuthInProduction. Both look right at a glance.
func TestCommentsDoNotCiteMissingTestFunctions(t *testing.T) {
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
		t.Fatal("found no test functions -- this guard would silently pass forever")
	}

	// Only match the explicit `file_test.go::TestName` citation form. A bare TestName in
	// prose is too easy to false-positive on.
	citeRE := regexp.MustCompile(`_test\.go::(Test[A-Za-z0-9_]+)`)

	for _, rel := range testutil.TrackedFiles(t) {
		if strings.HasPrefix(rel, "gen/") || rel == selfExcluded {
			continue
		}
		switch filepath.Ext(rel) {
		case ".go", ".proto", ".yml", ".yaml", ".md":
		default:
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		for _, m := range citeRE.FindAllStringSubmatch(string(b), -1) {
			if !declared[m[1]] {
				t.Errorf("%s cites test function %s, which does not exist", rel, m[1])
			}
		}
	}
}
