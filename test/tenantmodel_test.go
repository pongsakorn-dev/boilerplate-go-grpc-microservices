package test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/platform/migrations"
)

// THE THIRD GORM COMPENSATION, and the one that was missing.
//
// Choosing GORM over a query builder gave up compile-time column checking, and the plan named
// three mechanisms to buy it back. Two shipped: gormx/tenant.go's fail-closed callback, and
// orderpg's drift check against the goose schema. This is the third.
//
// WHAT THE EXISTING TEST DOES NOT COVER. gormx/tenant_test.go proves the callback BEHAVES --
// no tenant in context is an error, tenant A cannot read tenant B. Every one of its cases is
// built on a model that already implements gormx.TenantScoped. It therefore says nothing about
// the table somebody adds next.
//
// That gap is the whole failure mode. gormx.TenantScoped is an interface, so a new model is
// tenant-guarded only if someone remembers to write one four-word method. Forget it and there
// is no error, no warning and no failing test: the callback simply never fires for that model,
// every query against it returns every tenant's rows, and it looks like a working feature
// until a customer sees another customer's data.
//
// This walks the shipped schema instead of the models, which is the direction that catches
// what a reviewer misses -- the table exists in SQL whether or not anyone wrote the method.

// exemptTables are tenant-carrying tables whose Go model deliberately does NOT implement
// gormx.TenantScoped.
//
// An allowlist rather than a naming convention, because each entry is a decision that had a
// reason, and the reason is what a reader needs. Adding an entry here should feel like more
// work than adding the method -- that asymmetry is the point.
var exemptTables = map[string]string{
	"outbox": "The relay legitimately drains EVERY tenant's rows: it is infrastructure moving " +
		"events to a broker, not a caller reading data on someone's behalf. Scoping it to one " +
		"tenant would mean no tenant's events were ever published. It reaches the table through " +
		"raw SQL, which gormx's guard deliberately ignores (see tenantGuard's SQL.Len check).",
}

// TestEveryTenantScopedTableHasAGuardedModel is the structural guard.
func TestEveryTenantScopedTableHasAGuardedModel(t *testing.T) {
	t.Parallel()

	scoped := tenantCarryingTables(t)
	if len(scoped) == 0 {
		t.Fatal("no table in the migration set has a tenant_id column.\n\n" +
			"Either the schema stopped being multi-tenant or this test stopped being able to " +
			"read it; both make everything below meaningless.")
	}

	models := gormModels(t)

	for _, table := range scoped {
		model, ok := models[table]
		if !ok {
			// No GORM model means the table is reached by raw SQL only -- order_counts and
			// processed_events are both like this. gormx's guard never applies to raw SQL by
			// design, so there is nothing here to enforce.
			t.Logf("table %q carries a tenant but has no GORM model; it is reached by raw SQL "+
				"and the callback does not apply to it", table)
			continue
		}

		if reason, exempt := exemptTables[table]; exempt {
			if model.guarded {
				t.Errorf("table %q is listed in exemptTables, but %s DOES implement "+
					"gormx.TenantScoped.\n\nThe exemption is stale -- delete it, or the next "+
					"reader will believe this table is unguarded when it is not.\nreason on "+
					"file: %s", table, model.typeName, reason)
			}
			continue
		}

		if !model.guarded {
			t.Errorf("table %q has a tenant_id column and its model %s (%s) does not implement "+
				"gormx.TenantScoped.\n\n"+
				"Every Query, Update and Delete against this model will run WITHOUT a tenant "+
				"predicate. That does not fail -- it returns every tenant's rows, which reads "+
				"as a working feature.\n\n"+
				"Fix it with one method:\n\n"+
				"    func (%s) TenantColumn() string { return \"tenant_id\" }\n\n"+
				"or, if crossing tenants is genuinely intended, add %q to exemptTables with the "+
				"reason.",
				table, model.typeName, model.file, model.typeName, table)
		}
	}
}

// TestNoExemptionOutlivesItsTable keeps the allowlist honest from the other side.
//
// An entry naming a table that no longer exists, or that no longer carries a tenant, is a
// standing invitation to add a second one by copying it. Allowlists rot silently; this is the
// cheapest way to notice.
func TestNoExemptionOutlivesItsTable(t *testing.T) {
	t.Parallel()

	scoped := tenantCarryingTables(t)

	for table := range exemptTables {
		if !slices.Contains(scoped, table) {
			t.Errorf("exemptTables names %q, which is not a tenant-carrying table in the "+
				"migration set (found: %v).\n\nThe table was renamed, dropped, or lost its "+
				"tenant_id column. Remove the exemption.", table, scoped)
		}
	}
}

// gormModel is one Go type that maps to a table.
type gormModel struct {
	typeName string
	file     string
	guarded  bool // implements gormx.TenantScoped
}

var (
	createTableRe = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)\s*\(`)
	addColumnRe   = regexp.MustCompile(`(?is)ALTER\s+TABLE\s+([a-z_][a-z0-9_]*)\s+ADD\s+COLUMN\s+([a-z_][a-z0-9_]*)`)
	tenantColumn  = regexp.MustCompile(`(?im)^\s*tenant_id\b`)
)

// tenantCarryingTables reads the SHIPPED schema and returns every table with a tenant_id column.
//
// The migrations are the authority here, not the Go structs. Reading the structs to decide
// which tables are tenant-scoped would be circular: a model missing the method would also be
// missing from the list of things to check, and the test would pass by construction.
func tenantCarryingTables(t *testing.T) []string {
	t.Helper()

	entries, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("glob the embedded migrations: %v", err)
	}
	slices.Sort(entries) // a column can be added by a later file than the CREATE

	var tables []string
	for _, name := range entries {
		raw, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}

		// Only the Up half. The Down half drops and recreates things, and reading it would
		// report the schema as it exists during a rollback rather than as it ships.
		up, _, _ := strings.Cut(string(raw), "-- +goose Down")

		for _, m := range createTableRe.FindAllStringSubmatchIndex(up, -1) {
			table := up[m[2]:m[3]]
			body, ok := parenBody(up, m[1]-1)
			if !ok {
				t.Fatalf("%s: could not find the closing paren of CREATE TABLE %s", name, table)
			}
			if tenantColumn.MatchString(body) && !slices.Contains(tables, table) {
				tables = append(tables, table)
			}
		}

		for _, m := range addColumnRe.FindAllStringSubmatch(up, -1) {
			if m[2] == "tenant_id" && !slices.Contains(tables, m[1]) {
				tables = append(tables, m[1])
			}
		}
	}

	slices.Sort(tables)
	return tables
}

// parenBody returns the text between the paren at open and its match.
func parenBody(s string, open int) (string, bool) {
	if open < 0 || open >= len(s) || s[open] != '(' {
		return "", false
	}
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[open+1 : i], true
			}
		}
	}
	return "", false
}

// gormModels finds every type declaring TableName, and whether it also declares TenantColumn.
//
// The AST rather than reflection, because the models are unexported: orderpg.orderRow cannot
// be named from this package, and a registry that models had to opt into would be one more
// thing to forget -- the same defect this test exists to catch.
//
// The go/ast standard library rather than golang.org/x/tools/go/packages: nothing here needs
// type resolution, and adding a type-checking dependency to the default test tier for a string
// match would be a poor trade under this repository's dependency rules.
func gormModels(t *testing.T) map[string]gormModel {
	t.Helper()

	models := map[string]gormModel{}
	guarded := map[string]bool{} // "dir\ttype" -> declares TenantColumn
	tables := map[string]string{}

	fset := token.NewFileSet()

	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			// gen/ is machine-written and holds no GORM models; .git and the module cache
			// are not ours to read.
			case ".git", "gen", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		// Production models only. Including _test.go would let a fixture in a test file
		// satisfy the requirement for a real table.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
				continue
			}
			recv := receiverType(fn.Recv.List[0].Type)
			if recv == "" {
				continue
			}
			key := filepath.Dir(path) + "\t" + recv

			switch fn.Name.Name {
			case "TableName":
				if table, ok := returnedString(fn); ok {
					tables[key] = table
					models[table] = gormModel{typeName: recv, file: filepath.ToSlash(path)}
				}
			case "TenantColumn":
				guarded[key] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}

	for key, table := range tables {
		if guarded[key] {
			m := models[table]
			m.guarded = true
			models[table] = m
		}
	}
	return models
}

// receiverType returns the bare type name of a method receiver, unwrapping a pointer.
func receiverType(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// returnedString pulls the constant out of `func (T) TableName() string { return "x" }`.
//
// Reports false for anything computed. That is deliberate: a TableName this cannot read is
// also one a reader cannot check at a glance, and silently skipping it would drop the table
// out of the guard entirely.
func returnedString(fn *ast.FuncDecl) (string, bool) {
	if fn.Body == nil || len(fn.Body.List) != 1 {
		return "", false
	}
	ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return "", false
	}
	lit, ok := ret.Results[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}
