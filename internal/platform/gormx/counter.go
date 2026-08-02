package gormx

import (
	"sync"

	"gorm.io/gorm"
)

// QueryCounter counts the SQL statements a block of code issues.
//
// IT IS SHIPPED, not test-only, because N+1 is GORM's characteristic failure mode and it is
// invisible everywhere it matters. Loading fifty orders with their line items either costs
// two queries or fifty-one, and both versions return identical data, pass every functional
// test, and look the same in code review. The difference only shows up under load, as
// latency nobody can attribute.
//
// So the count becomes an assertion:
//
//	c := gormx.NewQueryCounter()
//	c.Attach(db)
//	... load 50 orders with items ...
//	if c.Count() != 2 { ... }
//
// Adopters should use it on their own hot paths. That is why it lives in a normal package
// rather than a _test.go file -- a guard that only the template's own tests can use teaches
// nothing.
type QueryCounter struct {
	mu       sync.Mutex
	count    int
	sql      []string
	executed []string
}

// NewQueryCounter returns a zeroed counter.
func NewQueryCounter() *QueryCounter { return &QueryCounter{} }

// Attach registers the counter's callbacks on db.
//
// Registered on the AFTER hooks of every operation, because that is where GORM has finished
// building Statement.SQL -- reading it in a Before callback yields an empty string, which
// would make this count correctly and record nothing.
func (c *QueryCounter) Attach(db *gorm.DB) error {
	cb := db.Callback()
	for name, register := range map[string]func(string, func(*gorm.DB)) error{
		"query":  cb.Query().After("gorm:query").Register,
		"create": cb.Create().After("gorm:create").Register,
		"update": cb.Update().After("gorm:update").Register,
		"delete": cb.Delete().After("gorm:delete").Register,
		"row":    cb.Row().After("gorm:row").Register,
		"raw":    cb.Raw().After("gorm:raw").Register,
	} {
		if err := register("gomicro:count_"+name, c.record); err != nil {
			return err
		}
	}
	return nil
}

func (c *QueryCounter) record(db *gorm.DB) {
	if db.Statement == nil {
		return
	}

	raw := db.Statement.SQL.String()

	// Also keep the INTERPOLATED form, with parameters substituted.
	//
	// Not for logging -- it is unsafe to execute and may contain personal data -- but so a
	// test can hand the real, executed query to EXPLAIN. Without it, a plan test has to
	// hand-write the SQL it believes the adapter emits, which makes it a test of the test:
	// changing the adapter's query then cannot fail it. That mistake was made here and
	// caught by deliberately rewriting the query and watching the suite stay green.
	interpolated := raw
	if db.Dialector != nil && len(db.Statement.Vars) > 0 {
		interpolated = db.Explain(raw, db.Statement.Vars...)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
	c.sql = append(c.sql, raw)
	c.executed = append(c.executed, interpolated)
}

// Executed returns the statements with their parameters substituted, in order.
//
// Use it to feed a real query to EXPLAIN. Do NOT log it or execute it against user input:
// interpolation is for inspection, and the values may be personal data.
func (c *QueryCounter) Executed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.executed...)
}

// Count returns the number of statements issued since the last Reset.
func (c *QueryCounter) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// Statements returns the SQL seen so far, so a failing count assertion can print what
// actually ran instead of just how many.
//
// A test that says "got 51 queries, want 2" sends you looking; one that also prints the
// fifty identical SELECTs on order_items tells you the answer.
func (c *QueryCounter) Statements() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sql...)
}

// Reset zeroes the counter, so setup queries do not count toward the assertion.
func (c *QueryCounter) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count = 0
	c.sql = nil
	c.executed = nil
}
