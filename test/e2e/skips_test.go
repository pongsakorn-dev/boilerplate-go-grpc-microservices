//go:build e2e

package e2e

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEveryEndToEndTestSkipsWhenTheStackIsMissing is the guard that keeps the skip honest.
//
// # Why a structural test rather than trusting the convention
//
// Both packages in this tier record why the stack could not start and let each test call
// t.Skip, so `go test` prints `--- SKIP:` lines a human or a grep can see. That replaced an
// early os.Exit(0), which printed
//
//	ok  github.com/example/gomicro/test/e2e   1.828s
//
// for a run that started no containers and asserted nothing -- indistinguishable from a pass,
// on the only tier covering the shipped images, the compose file, graceful shutdown and OIDC.
//
// The weakness of that design is that it depends on every test remembering to call
// requireStack. A test added later that forgets does not skip: it runs against nothing, fails
// on a connection refused, and the failure looks like a broken service rather than a missing
// daemon. Worse, someone debugging that will be tempted to "fix" it by skipping on error --
// which is how the false green comes back.
//
// So the convention is checked rather than trusted. This test needs no Docker and runs in
// milliseconds, which means it is also the one thing in this package that genuinely passes
// when the stack is absent.
func TestEveryEndToEndTestSkipsWhenTheStackIsMissing(t *testing.T) {
	// Both packages of the tier, checked from here so there is one guard rather than two.
	for _, dir := range []string{".", "oidc"} {
		missing := testsWithoutRequireStack(t, dir)
		if len(missing) == 0 {
			continue
		}
		sort.Strings(missing)
		t.Errorf("in test/e2e/%s, these tests do not call requireStack(t) first:\n\n  %s\n\n"+
			"Without it they run against a stack that may never have started -- failing on a "+
			"refused connection, which reads as a broken service rather than a missing Docker "+
			"daemon.", strings.TrimPrefix(dir, "."), strings.Join(missing, "\n  "))
	}
}

// testsWithoutRequireStack parses one directory's test files and returns the names of TestXxx
// functions whose body does not call requireStack.
//
// The AST rather than a grep: a grep matches the word in a comment, and this file is full of
// them.
func testsWithoutRequireStack(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	var missing []string
	var checked int

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}

		// ParseFile ignores build tags, which is what is wanted here: these files are only
		// ever built with -tags=e2e, and this guard has to read them regardless.
		file, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			// TestMain is the one that sets `unavailable`; it cannot skip itself.
			if !strings.HasPrefix(name, "Test") || name == "TestMain" {
				continue
			}
			// This guard is the exception, and deliberately so: it is the test that must run
			// when the stack is absent.
			if name == "TestEveryEndToEndTestSkipsWhenTheStackIsMissing" {
				continue
			}

			checked++
			if !callsRequireStack(fn.Body) {
				missing = append(missing, name)
			}
		}
	}

	if checked == 0 {
		t.Fatalf("found no end-to-end tests in %s; this guard would pass forever", dir)
	}
	return missing
}

// callsRequireStack reports whether a function body calls requireStack anywhere.
//
// Anywhere rather than as the first statement: a test that sets up a subtest table before
// skipping is still correct, and demanding a position would fail useful code for no gain.
func callsRequireStack(body *ast.BlockStmt) bool {
	found := false

	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "requireStack" {
			found = true
			return false
		}
		return true
	})

	return found
}
