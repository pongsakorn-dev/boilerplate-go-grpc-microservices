package grpcapi_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/testutil"
)

// declaredStreamGaps is the set of interceptors the unary chain has and the stream chain
// deliberately does not, as argued in chain.go.
//
// It is written here, once, and checked against the source. Adding an interceptor to the
// unary chain without either adding a streaming counterpart or adding it to this list is a
// test failure that says which one you did.
var declaredStreamGaps = []string{
	// Holds a concurrency slot for the life of the call. Ten idle watchers would exhaust a
	// limiter sized for the database pool -- worse than not limiting them at all.
	"Admission",

	// A unary interceptor sees one request message. A stream's messages arrive inside the
	// handler, so per-message validation has to live there.
	"Validate",
}

// TestTheStreamChainGapsAreTheDeclaredOnes turns a paragraph of prose into an assertion.
//
// chain.go carries a comment whose entire job is to enumerate what the stream chain does not
// have and why each absence is deliberate. It said "no Admission and no Validate" while the
// chain was ALSO missing RateLimit -- so the one comment written specifically to catalogue
// the gaps had a gap in it, and the effect was worse than no comment: a reader auditing the
// stream path found a considered-looking list and stopped looking.
//
// That omission was a live bypass. WatchOrders runs a List against the database on every
// open, so a tenant that had spent its quota on the unary ListOrders could keep issuing the
// same query by opening streams instead.
//
// Reading the source is the point. A behavioural test proves an interceptor is present; only
// this can prove the DOCUMENTED SET is complete, which is the thing that actually drifted.
func TestTheStreamChainGapsAreTheDeclaredOnes(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)
	path := filepath.Join(root, "internal", "grpcapi", "chain.go")

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse chain.go: %v", err)
	}

	unary := chainInterceptors(t, file, "unary")
	stream := chainInterceptors(t, file, "stream")

	if len(unary) == 0 || len(stream) == 0 {
		t.Fatalf("found unary=%v stream=%v -- the slices were not recognised, so this guard "+
			"would pass forever. chain.go's shape has changed.", unary, stream)
	}

	var gaps []string
	for _, name := range unary {
		if !slices.Contains(stream, name) {
			gaps = append(gaps, name)
		}
	}
	slices.Sort(gaps)

	want := slices.Clone(declaredStreamGaps)
	slices.Sort(want)

	if !slices.Equal(gaps, want) {
		t.Errorf("the stream chain is missing %v; the declared gaps are %v.\n\n"+
			"Every difference between the chains must be a decision somebody wrote down.\n"+
			"If the new gap is intended, add it to declaredStreamGaps WITH the reason, and\n"+
			"say the same thing in chain.go. If it is not, add the streaming counterpart --\n"+
			"an interceptor that runs on unary calls and not on streams is a bypass that any\n"+
			"caller can take by choosing a streaming RPC.",
			gaps, want)
	}

	// The reverse direction: a stream-only interceptor would be just as surprising, and
	// nothing else would report it.
	for _, name := range stream {
		if !slices.Contains(unary, name) {
			t.Errorf("%s runs on streams but not on unary calls, which is almost certainly "+
				"backwards", name)
		}
	}
}

// chainInterceptors returns the base interceptor names in the named slice literal.
//
// "Base" means the Stream suffix is removed, so RecoveryStream and Recovery compare equal --
// the question being asked is which CONCERNS each chain covers, not which symbols it names.
// Non-interceptor entries (the metrics interceptor comes off a struct field) are matched by
// selector text rather than skipped, so a chain cannot lose one silently.
func chainInterceptors(t *testing.T, file *ast.File, varName string) []string {
	t.Helper()

	var found []string

	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		ident, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || ident.Name != varName {
			return true
		}
		lit, ok := assign.Rhs[0].(*ast.CompositeLit)
		if !ok {
			return true
		}

		for _, elt := range lit.Elts {
			call, ok := elt.(*ast.CallExpr)
			if !ok {
				continue
			}
			if name := baseName(call.Fun); name != "" {
				found = append(found, name)
			}
		}
		return false
	})

	return found
}

// baseName extracts "Recovery" from interceptor.RecoveryStream(...) and "Metrics" from
// d.Metrics.Server.StreamServerInterceptor().
func baseName(fun ast.Expr) string {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}

	// The grpcprom interceptors are methods on a metrics struct rather than functions in
	// the interceptor package, so they are named by what they observe.
	if strings.HasSuffix(sel.Sel.Name, "ServerInterceptor") {
		return "Metrics"
	}

	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "interceptor" {
		return ""
	}
	return strings.TrimSuffix(sel.Sel.Name, "Stream")
}
