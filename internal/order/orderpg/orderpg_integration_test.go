//go:build integration

package orderpg_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"gorm.io/gorm"

	"github.com/example/gomicro/internal/order"
	"github.com/example/gomicro/internal/order/orderpg"
	"github.com/example/gomicro/internal/order/ordertest"
	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/platform/gormx"
	"github.com/example/gomicro/internal/platform/migrations"
	"github.com/example/gomicro/internal/platform/testdb"
)

// instance is the ONE container for this whole package.
//
// Per-package rather than per-test: starting Postgres costs a few seconds, and paying that
// per test turns a suite people run into a suite people skip. Isolation comes from cloning a
// pre-migrated template database instead, which costs milliseconds.
var instance *testdb.Instance

func TestMain(m *testing.M) {
	ctx := context.Background()

	inst, err := testdb.Start(ctx)
	if err != nil {
		// Not a Fatal: TestMain cannot skip, so an unreachable Docker daemon must not look
		// like a broken build. Each test calls RequireDocker and skips itself with an
		// actionable message; this just lets them get that far.
		fmt.Fprintf(os.Stderr, "testdb.Start: %v\n", err)
		os.Exit(m.Run())
	}
	instance = inst

	code := m.Run()
	_ = inst.Terminate(ctx)
	os.Exit(code)
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fixture is one test's private database, opened two ways.
//
// Both views point at the SAME database, which is what lets a test seed through the store,
// count queries through GORM, and read a query plan through database/sql without any of them
// seeing another test's rows.
type fixture struct {
	cfg   config.Config
	db    *gorm.DB
	store *orderpg.Store
}

// newFixture clones the migrated template into a fresh database and wires the adapter.
func newFixture(t *testing.T) fixture {
	t.Helper()

	testdb.RequireDocker(t)
	if instance == nil {
		t.Skip("no database instance; see the TestMain output above")
	}

	cfg, err := config.Parse(map[string]string{
		"STORE_DRIVER": config.StorePostgres,
		"POSTGRES_DSN": instance.Fresh(t),
	})
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}

	db, err := gormx.Open(context.Background(), cfg, discard())
	if err != nil {
		t.Fatalf("gormx.Open: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	return fixture{cfg: cfg, db: db, store: orderpg.New(db)}
}

// sqlDB opens a plain database/sql handle on the same database, for EXPLAIN and schema
// introspection that GORM would only get in the way of.
func (f fixture) sqlDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("pgx", f.cfg.Postgres.DSN.Reveal())
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func (f fixture) harness() ordertest.Harness {
	return ordertest.Harness{Store: f.store, Atomic: f.store, Events: f.store.Events}
}

// seed inserts n orders one millisecond apart, so the keyset sort key is well spread.
func (f fixture) seed(t *testing.T, n int) time.Time {
	t.Helper()

	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Millisecond)
	for i := range n {
		o := ordertest.NewOrder(
			ordertest.WithID(ordertest.SeqID(i+1)),
			ordertest.WithCreatedAt(base.Add(time.Duration(i)*time.Millisecond)),
		)
		if err := f.store.Create(ctx, o); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	return base
}

// TestStoreContract is the most valuable test in this package, and it contains no assertions
// of its own.
//
// It runs the SAME suite that internal/order/ordermem runs, unchanged, against real
// Postgres. That turns "the in-memory fake behaves like the database" from an assumption into
// a tested property -- and a fake nothing holds to a contract is just a second implementation
// of your bugs, which every unit test above it then cheerfully agrees with.
func TestStoreContract(t *testing.T) {
	ordertest.RunStoreContract(t, func(t *testing.T) ordertest.Harness {
		return newFixture(t).harness()
	})
}

// Everything below asserts something the in-memory store CANNOT. That is the rule for what
// belongs here rather than in the shared contract.

// TestDuplicateIsDetectedBySQLSTATE pins the mapping to the error CODE, not the message.
//
// Message text is localised, changes between Postgres releases, and differs between drivers.
// A store that string-matches "duplicate key value violates unique constraint" works until
// somebody upgrades, at which point every duplicate becomes an Internal and clients get a 500
// for what should be a 409.
func TestDuplicateIsDetectedBySQLSTATE(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	o := ordertest.NewOrder(ordertest.WithID(ordertest.SeqID(1)))
	if err := f.store.Create(ctx, o); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	if err := f.store.Create(ctx, o); !errors.Is(err, order.ErrDuplicate) {
		t.Fatalf("second Create returned %v, want order.ErrDuplicate", err)
	}
}

// TestListDoesNotNPlusOne is the guard for GORM's characteristic failure.
//
// Loading fifty orders with their line items either costs two queries or fifty-one. Both
// return identical data, pass every functional test, and read the same in review; the
// difference appears only under load, as latency nobody can attribute. Counting is the only
// way to see it, which is why gormx ships the counter as normal code rather than a test
// helper.
func TestListDoesNotNPlusOne(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	const n = 50
	f.seed(t, n)

	// Attached after seeding, so the inserts do not count.
	counter := gormx.NewQueryCounter()
	if err := counter.Attach(f.db); err != nil {
		t.Fatalf("attach counter: %v", err)
	}

	page, err := f.store.List(ctx, ordertest.RefTenant, order.ListFilter{PageSize: n})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Orders) != n {
		t.Fatalf("got %d orders, want %d", len(page.Orders), n)
	}
	for _, o := range page.Orders {
		if len(o.Items) == 0 {
			t.Fatal("an order came back with no items, so the preload did not happen and the " +
				"query count below would be meaningless")
		}
	}

	// Two: one SELECT for the orders, one IN-query for all of their items.
	if got := counter.Count(); got != 2 {
		t.Errorf("List issued %d queries for %d orders, want 2 (one for orders, one for items).\n\n"+
			"This is the N+1 GORM makes easy and invisible.\n\nStatements:\n  %s",
			got, n, strings.Join(counter.Statements(), "\n  "))
	}
}

// TestKeysetPaginationSeeksRatherThanFilters asserts the PLAN of the query the ADAPTER
// ACTUALLY ISSUES, and asserts the right thing about it. Both halves were wrong first time.
//
// Wrong thing #1: it EXPLAINed SQL the test itself had written. That makes it a test of the
// test -- rewriting the adapter's predicate to the OR form left the suite green. Verified by
// doing exactly that. The query now comes from the statement GORM emitted.
//
// Wrong thing #2: it asserted only that the plan named orders_keyset_idx. Measuring showed
// the OR form ALSO uses that index -- it seeks on tenant_id alone and pushes the sort key
// into a Filter, scanning every row the tenant owns:
//
//	row-value  Index Cond: tenant_id = ... AND ROW(created_at, id) > ROW(...)
//	OR form    Index Cond: tenant_id = ...
//	           Filter:     created_at > ... OR (created_at = ... AND id > ...)
//
// Same rows, same index, and one of them is the linear scan keyset pagination exists to
// avoid. So the assertion is on the INDEX CONDITION carrying the sort key.
func TestKeysetPaginationSeeksRatherThanFilters(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.seed(t, 200)

	// Page once to get a real cursor, so the captured query is the RESUME query rather than
	// the first page, which has no keyset predicate at all.
	first, err := f.store.List(ctx, ordertest.RefTenant, order.ListFilter{PageSize: 50})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if first.NextPageToken == "" {
		t.Fatal("no next page token, so there is no keyset query to inspect")
	}

	counter := gormx.NewQueryCounter()
	if err := counter.Attach(f.db); err != nil {
		t.Fatalf("attach counter: %v", err)
	}
	if _, err := f.store.List(ctx, ordertest.RefTenant, order.ListFilter{
		PageSize:  50,
		PageToken: first.NextPageToken,
	}); err != nil {
		t.Fatalf("second page: %v", err)
	}

	query := ""
	for _, stmt := range counter.Executed() {
		if strings.Contains(stmt, "orders") && strings.HasPrefix(strings.ToUpper(stmt), "SELECT") {
			query = stmt
			break
		}
	}
	if query == "" {
		t.Fatalf("did not capture a SELECT on orders.\nStatements:\n  %s",
			strings.Join(counter.Executed(), "\n  "))
	}
	t.Logf("adapter query: %s", query)

	db := f.sqlDB(t)

	// ANALYZE first: a freshly-created table has no statistics, so the planner would
	// sequential-scan regardless of the index and this test would fail for a reason that has
	// nothing to do with the query.
	if _, err := db.ExecContext(ctx, "ANALYZE orders"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
	// And discourage the seq scan the planner still (correctly) prefers on 200 rows. The
	// question is whether the index CAN serve this predicate, not what a tiny table costs.
	if _, err := db.ExecContext(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("SET enable_seqscan: %v", err)
	}

	plan := explain(t, db, query)

	if !strings.Contains(plan, "orders_keyset_idx") {
		t.Fatalf("the keyset query does not use orders_keyset_idx at all.\n\nPlan:\n%s", plan)
	}

	indexCond := planLine(plan, "Index Cond:")
	if !strings.Contains(indexCond, "created_at") {
		t.Errorf("the sort key is not in the index condition.\n\n"+
			"Index Cond: %s\n\nWithout it the scan seeks on tenant_id alone and filters every "+
			"row that tenant owns -- the linear scan keyset pagination exists to avoid, "+
			"reintroduced by a rewrite that looks equivalent.\n\nPlan:\n%s", indexCond, plan)
	}

	if filter := planLine(plan, "Filter:"); strings.Contains(filter, "created_at") {
		t.Errorf("created_at is filtered rather than seeked: %s\n\nPlan:\n%s", filter, plan)
	}
}

// TestMigrationsRoundTrip proves the Down blocks actually work.
//
// A Down block nobody runs is a Down block that does not work, and you discover that in the
// middle of a failed release. Up, all the way back to 0, then up again must produce an
// identical schema; if it does not, the rollback path is a trap rather than an escape.
func TestMigrationsRoundTrip(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	db := f.sqlDB(t)

	before := schemaSnapshot(t, db)
	if before == "" {
		t.Fatal("the migrated schema is empty, so this comparison would be vacuous")
	}

	if err := migrations.DownTo(ctx, db, 0); err != nil {
		t.Fatalf("roll back to 0: %v", err)
	}
	if tables := tableNames(t, db); len(tables) != 0 {
		t.Errorf("after rolling back to 0 these tables remain: %v", tables)
	}

	if err := migrations.Up(ctx, db); err != nil {
		t.Fatalf("re-apply: %v", err)
	}

	if after := schemaSnapshot(t, db); before != after {
		t.Errorf("up -> down-to-0 -> up produced a DIFFERENT schema.\n\nbefore:\n%s\nafter:\n%s",
			before, after)
	}
}

// TestTenantGuardFailsClosed is the assertion that justifies the GORM callback existing.
//
// The domain Store takes tenantID explicitly, so the adapter's own WHERE clause already
// scopes every query. This tests the SECOND line of defence: a model query issued with no
// tenant in context must ERROR rather than quietly return every tenant's rows.
//
// That failure mode is the worst a multi-tenant service has, and it looks like a working
// feature -- the query succeeds, the response is well-formed, and it contains other
// customers' data.
func TestTenantGuardFailsClosed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if err := f.store.Create(ctx, ordertest.NewOrder(ordertest.WithID(ordertest.SeqID(1)))); err != nil {
		t.Fatalf("seed tenant A: %v", err)
	}
	theirs := ordertest.NewOrder(ordertest.WithID(ordertest.SeqID(2)), ordertest.WithTenant("tenant-b"))
	if err := f.store.Create(ctx, theirs); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}

	t.Run("List with an empty tenant fails closed", func(t *testing.T) {
		if _, err := f.store.List(ctx, "", order.ListFilter{}); err == nil {
			t.Error("List with an empty tenant succeeded; it must fail closed")
		}
	})

	t.Run("a tenant sees only its own rows", func(t *testing.T) {
		page, err := f.store.List(ctx, ordertest.RefTenant, order.ListFilter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, o := range page.Orders {
			if o.TenantID != ordertest.RefTenant {
				t.Fatalf("List for %q returned an order from %q", ordertest.RefTenant, o.TenantID)
			}
		}
		if len(page.Orders) != 1 {
			t.Errorf("got %d orders, want 1 -- the other belongs to tenant-b", len(page.Orders))
		}
	})

	t.Run("the guard rejects a tenant-scoped query with no tenant context", func(t *testing.T) {
		// The guard's own contract, exercised directly rather than through the adapter.
		var rows []guardProbe
		err := f.db.WithContext(ctx).Model(&guardProbe{}).Find(&rows).Error
		if !errors.Is(err, gormx.ErrNoTenantInContext) {
			t.Errorf("a tenant-scoped query with no tenant returned err=%v and %d rows, "+
				"want ErrNoTenantInContext.\n\n"+
				"Without the guard this query succeeds and returns EVERY tenant's rows.",
				err, len(rows))
		}
	})

	t.Run("the guard admits the same query once a tenant is present", func(t *testing.T) {
		var rows []guardProbe
		err := f.db.WithContext(gormx.WithTenant(ctx, ordertest.RefTenant)).
			Model(&guardProbe{}).Find(&rows).Error
		if err != nil {
			t.Fatalf("a scoped query failed: %v", err)
		}
		if len(rows) != 1 {
			t.Errorf("got %d rows, want 1 -- the guard should have injected tenant_id = %q",
				len(rows), ordertest.RefTenant)
		}
	})
}

// guardProbe is a minimal tenant-scoped model over the orders table, so the guard can be
// tested directly without exporting orderpg's row type.
type guardProbe struct {
	ID       string `gorm:"column:id;primaryKey"`
	TenantID string `gorm:"column:tenant_id"`
}

func (guardProbe) TableName() string    { return "orders" }
func (guardProbe) TenantColumn() string { return "tenant_id" }

// --- helpers -------------------------------------------------------------------------

func explain(t *testing.T, db *sql.DB, query string) string {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), "EXPLAIN "+query)
	if err != nil {
		t.Fatalf("EXPLAIN: %v\nquery: %s", err, query)
	}
	defer func() { _ = rows.Close() }()

	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		b.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return b.String()
}

// planLine returns the first plan line containing prefix, or "".
func planLine(plan, prefix string) string {
	for _, line := range strings.Split(plan, "\n") {
		if strings.Contains(line, prefix) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// schemaSnapshot renders every column of every table, so two schemas compare as text.
func schemaSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `
		SELECT table_name, column_name, data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name <> 'goose_db_version'
		ORDER BY table_name, ordinal_position`)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var b strings.Builder
	for rows.Next() {
		var table, column, dataType, nullable string
		if err := rows.Scan(&table, &column, &dataType, &nullable); err != nil {
			t.Fatalf("scan schema: %v", err)
		}
		fmt.Fprintf(&b, "%s.%s %s null=%s\n", table, column, dataType, nullable)
	}
	return b.String()
}

func tableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name <> 'goose_db_version'
		ORDER BY table_name`)
	if err != nil {
		t.Fatalf("read tables: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		out = append(out, name)
	}
	return out
}
