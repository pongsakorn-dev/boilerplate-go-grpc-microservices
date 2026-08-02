package gormx

import (
	"context"
	"testing"
)

// The guard's END-TO-END behaviour -- that an unscoped query errors instead of returning
// every tenant's rows -- is asserted against real Postgres in
// internal/order/orderpg/orderpg_integration_test.go::TestTenantGuardFailsClosed. It needs a
// database, because "what SQL did this produce" is the question.
//
// What is tested HERE is the type unwrapping, which needs no database and is the part most
// likely to fail silently. GORM populates Statement.Model and Statement.Dest with a struct, a
// pointer, a slice, or a pointer to a slice depending on which API the caller used. A version
// of tenantScopedOf that handles only the pointer-to-struct case still passes every Get test
// -- and stops applying to Find(&[]orderRow{}), which is the query that returns everybody's
// rows.

type scopedModel struct {
	ID       string
	TenantID string
}

func (scopedModel) TenantColumn() string { return "tenant_id" }

// ptrScopedModel declares the method on the POINTER receiver, which is legal, idiomatic, and
// invisible to a type assertion on the value type.
type ptrScopedModel struct{ ID string }

func (*ptrScopedModel) TenantColumn() string { return "org_id" }

type plainModel struct{ ID string }

func TestTenantScopedOfUnwrapsEveryShapeGORMProduces(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		value      any
		wantScoped bool
		wantColumn string
	}{
		{"struct value", scopedModel{}, true, "tenant_id"},
		{"pointer to struct", &scopedModel{}, true, "tenant_id"},

		// The dangerous ones. Find(&[]T{}) is the LIST query -- the one whose failure mode is
		// returning every tenant's rows rather than one missing row.
		{"slice of structs", []scopedModel{}, true, "tenant_id"},
		{"pointer to slice", &[]scopedModel{}, true, "tenant_id"},
		{"slice of pointers", []*scopedModel{}, true, "tenant_id"},
		{"pointer to slice of pointers", &[]*scopedModel{}, true, "tenant_id"},

		// Method on the pointer receiver: reflect.New is what finds it. Asserting on the
		// value type alone would miss this and silently leave the model unguarded.
		{"pointer-receiver method", []ptrScopedModel{}, true, "org_id"},

		{"unscoped struct", plainModel{}, false, ""},
		{"unscoped slice", &[]plainModel{}, false, ""},
		{"nil", nil, false, ""},
		{"map destination", &map[string]any{}, false, ""},
		{"primitive", new(int64), false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			scoped, ok := tenantScopedOf(tc.value)
			if ok != tc.wantScoped {
				t.Fatalf("tenantScopedOf(%T) = %v, want %v.\n\n"+
					"A shape the guard does not recognise is a query it does not scope, and an "+
					"unscoped query on a tenant table returns every tenant's rows.",
					tc.value, ok, tc.wantScoped)
			}
			if ok && scoped.TenantColumn() != tc.wantColumn {
				t.Errorf("TenantColumn() = %q, want %q", scoped.TenantColumn(), tc.wantColumn)
			}
		})
	}
}

// TestTenantScopedOfChecksEveryCandidate covers the real call, which passes both
// Statement.Model and Statement.Dest.
//
// GORM populates them differently per API: Find(&dest) leaves Model nil, while
// Model(&T{}).Updates(...) leaves Dest holding the update map. Checking only the first
// non-nil one would leave whichever half GORM chose today working and the other unguarded.
func TestTenantScopedOfChecksEveryCandidate(t *testing.T) {
	t.Parallel()

	// Model nil, Dest scoped -- the Find(&dest) shape.
	if _, ok := tenantScopedOf(nil, &[]scopedModel{}); !ok {
		t.Error("a scoped Dest was missed when Model was nil")
	}

	// Model scoped, Dest a map -- the Model(&T{}).Updates(map) shape.
	if _, ok := tenantScopedOf(&scopedModel{}, &map[string]any{}); !ok {
		t.Error("a scoped Model was missed when Dest was a map")
	}

	if _, ok := tenantScopedOf(nil, nil); ok {
		t.Error("two nils reported a scoped model")
	}
}

func TestTenantContextRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	if _, ok := TenantFromContext(ctx); ok {
		t.Error("an empty context reported a tenant")
	}

	// The empty string is NOT a tenant. Treating it as one would let a caller that forgot to
	// populate the tenant produce `WHERE tenant_id = ''`, which matches nothing and reads in
	// a log exactly like a legitimate empty result.
	if _, ok := TenantFromContext(WithTenant(ctx, "")); ok {
		t.Error("an empty tenant string was accepted as a tenant")
	}

	got, ok := TenantFromContext(WithTenant(ctx, "acme"))
	if !ok || got != "acme" {
		t.Errorf("TenantFromContext = (%q, %v), want (\"acme\", true)", got, ok)
	}

	//nolint:staticcheck // deliberately passing a nil context: the guard runs on whatever
	// GORM hands it, and a nil Statement.Context must not panic the process.
	if _, ok := TenantFromContext(nil); ok {
		t.Error("a nil context reported a tenant")
	}
}
