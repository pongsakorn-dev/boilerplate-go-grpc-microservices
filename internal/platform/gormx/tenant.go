package gormx

import (
	"context"
	"errors"
	"reflect"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrNoTenantInContext is returned when a tenant-scoped model is queried with no tenant.
//
// It is an ERROR, not an empty result and not "all rows". That choice is the entire point of
// this file: the failure mode of a forgotten tenant filter is a query that succeeds and
// returns every tenant's data, which looks like a working feature right up until someone
// notices their competitor's orders in the response.
var ErrNoTenantInContext = errors.New("tenant-scoped query with no tenant in context")

// TenantScoped marks a model whose rows belong to a tenant.
//
// An interface rather than a struct mixin so a model can name a column that is not literally
// "tenant_id", and so implementing it is a deliberate one-line act rather than something
// inherited by accident.
type TenantScoped interface {
	// TenantColumn is the column holding the tenant id.
	TenantColumn() string
}

type tenantKey struct{}

// WithTenant returns a context carrying the tenant for subsequent GORM operations.
//
// Adapters must call this. The filter is then applied TWICE: once in the adapter's own WHERE
// clause and once by the callback below.
//
// That redundancy is deliberate, and it was earned rather than assumed. An earlier version of
// orderpg.List wrote no tenant predicate of its own and relied on this callback alone. The
// comment here claimed "twice" anyway. Deleting the callback's registration and re-running the
// integration suite showed the cost: List returned BOTH tenants' rows, and only the guard's
// own test noticed -- the store contract's cross-tenant assertions all still passed.
//
// One mechanism whose removal silently produces cross-tenant reads is not defence in depth.
// So the adapter now writes the predicate explicitly, which makes each query correct and
// readable on its own, and this callback catches the query somebody forgets to write it on.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantKey{}, tenantID)
}

// TenantFromContext returns the tenant, if any.
func TenantFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	t, ok := ctx.Value(tenantKey{}).(string)
	return t, ok && t != ""
}

// RegisterTenantGuard installs the fail-closed tenant filter.
//
// WHY A CALLBACK RATHER THAN A LINT. GORM gives up the compile-time column checking that a
// query builder like sqlc provides, so there are no .sql files to scan and no generated
// method that can be made to require a tenant. A convention enforced only by code review is
// enforced until the first hurried Friday.
//
// A callback cannot be forgotten at a call site, because there is no call site: it runs for
// every Query, Update and Delete on any model implementing TenantScoped. The cost is that it
// is invisible in the code you read, which is why this comment is long and why
// tenant_test.go asserts the behaviour rather than the registration.
//
// Create is deliberately NOT guarded. An INSERT supplies tenant_id as a value rather than a
// filter, so there is nothing to inject; a row with the wrong tenant is a mapping bug the
// store contract catches ("writes without a tenant fail closed").
func RegisterTenantGuard(db *gorm.DB) error {
	cb := db.Callback()

	if err := cb.Query().Before("gorm:query").Register("gomicro:tenant_query", tenantGuard); err != nil {
		return err
	}
	if err := cb.Update().Before("gorm:update").Register("gomicro:tenant_update", tenantGuard); err != nil {
		return err
	}
	if err := cb.Delete().Before("gorm:delete").Register("gomicro:tenant_delete", tenantGuard); err != nil {
		return err
	}
	if err := cb.Row().Before("gorm:row").Register("gomicro:tenant_row", tenantGuard); err != nil {
		return err
	}
	return nil
}

// tenantGuard injects the tenant predicate, or fails the statement.
func tenantGuard(db *gorm.DB) {
	if db.Statement == nil {
		return
	}

	// A raw SQL statement carries no model, so there is nothing to scope. Raw SQL is the
	// documented escape hatch from this guard; orderpg uses it only for the outbox, which
	// is not tenant-scoped in the same sense (its rows carry a tenant but are drained by a
	// relay that legitimately reads across all of them).
	if db.Statement.SQL.Len() > 0 {
		return
	}

	scoped, ok := tenantScopedOf(db.Statement.Model, db.Statement.Dest)
	if !ok {
		return
	}

	tenant, ok := TenantFromContext(db.Statement.Context)
	if !ok {
		// AddError rather than panic: GORM aggregates statement errors and returns them
		// from the calling method, so the caller sees a normal error instead of a crash.
		_ = db.AddError(ErrNoTenantInContext)
		return
	}

	db.Statement.AddClause(clause.Where{Exprs: []clause.Expression{
		clause.Eq{
			Column: clause.Column{Table: clause.CurrentTable, Name: scoped.TenantColumn()},
			Value:  tenant,
		},
	}})
}

// tenantScopedOf finds a TenantScoped model in whatever GORM was handed.
//
// Statement.Model and Statement.Dest can each be a struct, a pointer to one, a slice, or a
// pointer to a slice, depending on which API the caller used -- First, Find, Model().Updates
// and Delete all populate them differently. Handling only the pointer-to-struct case is how
// this guard would silently stop applying to Find(&[]Order{}), which is exactly the query
// that returns every tenant's rows.
func tenantScopedOf(candidates ...any) (TenantScoped, bool) {
	for _, c := range candidates {
		if c == nil {
			continue
		}
		if ts, ok := c.(TenantScoped); ok {
			return ts, true
		}
		if ts, ok := elemOf(reflect.TypeOf(c)); ok {
			return ts, true
		}
	}
	return nil, false
}

// elemOf unwraps pointers, slices and arrays down to the element type and asks whether a
// zero value of it implements TenantScoped.
func elemOf(t reflect.Type) (TenantScoped, bool) {
	for t != nil {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			t = t.Elem()
		default:
			// reflect.New gives an addressable zero value, so a method declared on the
			// POINTER receiver is found too. Checking the value type alone would miss it.
			if ts, ok := reflect.New(t).Interface().(TenantScoped); ok {
				return ts, true
			}
			return nil, false
		}
	}
	return nil, false
}
