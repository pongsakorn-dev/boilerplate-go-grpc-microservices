package order

import (
	"context"
	"fmt"
	"time"
)

// SeedTenant is the tenant the dev principal acts as. Seed data belongs to it so that a
// fresh `go run ./cmd/orderd` followed by `grpcurl ... ListOrders` returns rows rather
// than an empty page -- an empty first response looks broken even when it is correct.
const SeedTenant = "dev-tenant"

// Seed writes a small, deterministic set of orders.
//
// It runs only for STORE_DRIVER=memory. It is deliberately NOT wired for Postgres: a
// service that writes fixture rows into a real database on boot will eventually do it to
// production, and "where did these orders come from" is a bad afternoon.
func Seed(ctx context.Context, store Store, at time.Time) error {
	price := func(units int64, nanos int32) Money {
		m, err := NewMoney("USD", units, nanos)
		if err != nil {
			panic("order.Seed: " + err.Error())
		}
		return m
	}

	seeds := []struct {
		id       string
		customer string
		status   Status
		items    []Item
	}{
		{
			id:       "01930000-0000-7000-8000-000000000001",
			customer: "customer-alice",
			status:   StatusPending,
			items: []Item{
				{SKU: "WIDGET-BLUE", Quantity: 2, UnitPrice: price(19, 990000000)},
				{SKU: "GIZMO-SMALL", Quantity: 1, UnitPrice: price(4, 500000000)},
			},
		},
		{
			id:       "01930000-0000-7000-8000-000000000002",
			customer: "customer-bob",
			status:   StatusConfirmed,
			items: []Item{
				{SKU: "WIDGET-RED", Quantity: 1, UnitPrice: price(24, 0)},
			},
		},
		{
			id:       "01930000-0000-7000-8000-000000000003",
			customer: "customer-alice",
			status:   StatusCancelled,
			items: []Item{
				{SKU: "DOODAD", Quantity: 10, UnitPrice: price(0, 990000000)},
			},
		},
	}

	for i, s := range seeds {
		total, err := ComputeTotal(s.items)
		if err != nil {
			return fmt.Errorf("seed %s: %w", s.id, err)
		}
		// Distinct timestamps so the seeded data exercises keyset pagination in a
		// meaningful order rather than relying on tiebreak behaviour.
		createdAt := at.Add(time.Duration(i) * time.Minute).UTC()

		o := Order{
			ID:         s.id,
			TenantID:   SeedTenant,
			CustomerID: s.customer,
			Status:     s.status,
			Items:      s.items,
			Total:      total,
			CreatedAt:  createdAt,
			UpdatedAt:  createdAt,
		}
		if err := o.Validate(); err != nil {
			return fmt.Errorf("seed %s is invalid: %w", s.id, err)
		}
		if err := store.Create(ctx, o); err != nil {
			return fmt.Errorf("seed %s: %w", s.id, err)
		}
	}
	return nil
}
