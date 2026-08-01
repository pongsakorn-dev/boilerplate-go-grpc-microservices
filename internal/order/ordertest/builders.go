// Package ordertest provides test data builders and the shared store contract.
//
// It is separate from ordermem on purpose: this package imports "testing", and ordermem
// backs the STORE_DRIVER=memory production path. Keeping them apart means no production
// binary ever links the testing package.
package ordertest

import (
	"time"

	"github.com/example/gomicro/internal/order"
)

// Fixed reference values. Tests that need a timestamp use these rather than time.Now(),
// so a failure message is the same on every run and on every machine.
//
// The time is chosen to have MICROSECOND precision and no finer. Postgres timestamptz
// stores microseconds, so a nanosecond-precision fixture would round-trip unequal through
// the real database and pass only against the in-memory store -- exactly the kind of
// divergence the shared contract exists to catch.
var (
	RefTime   = time.Date(2026, 3, 14, 15, 9, 26, 535000*1000, time.UTC)
	RefTenant = "tenant-a"
	OtherTen  = "tenant-b"
)

// USD builds a US dollar amount, e.g. USD(19, 990000000) is $19.99.
func USD(units int64, nanos int32) order.Money {
	m, err := order.NewMoney("USD", units, nanos)
	if err != nil {
		panic("ordertest.USD: " + err.Error())
	}
	return m
}

// Item builds a line item.
func Item(sku string, quantity int32, unitPrice order.Money) order.Item {
	return order.Item{SKU: sku, Quantity: quantity, UnitPrice: unitPrice}
}

// OrderOption customises the order built by NewOrder.
type OrderOption func(*order.Order)

func WithID(id string) OrderOption       { return func(o *order.Order) { o.ID = id } }
func WithTenant(id string) OrderOption   { return func(o *order.Order) { o.TenantID = id } }
func WithCustomer(id string) OrderOption { return func(o *order.Order) { o.CustomerID = id } }

func WithStatus(s order.Status) OrderOption { return func(o *order.Order) { o.Status = s } }

func WithCreatedAt(t time.Time) OrderOption {
	return func(o *order.Order) {
		o.CreatedAt = t
		o.UpdatedAt = t
	}
}

func WithItems(items ...order.Item) OrderOption {
	return func(o *order.Order) { o.Items = items }
}

// NewOrder builds a valid order. The total is always recomputed from the items, so a
// builder can never produce an order that fails its own Validate -- a fixture that is
// invalid by construction wastes a lot of debugging time.
func NewOrder(opts ...OrderOption) order.Order {
	o := order.Order{
		ID:         "00000000-0000-7000-8000-000000000001",
		TenantID:   RefTenant,
		CustomerID: "customer-1",
		Status:     order.StatusPending,
		Items:      []order.Item{Item("SKU-1", 2, USD(19, 990000000))},
		CreatedAt:  RefTime,
		UpdatedAt:  RefTime,
	}
	for _, opt := range opts {
		opt(&o)
	}
	total, err := order.ComputeTotal(o.Items)
	if err != nil {
		panic("ordertest.NewOrder: " + err.Error())
	}
	o.Total = total
	return o
}

// SeqID builds a sortable v7-shaped uuid for tests that need N distinct, ordered ids.
func SeqID(n int) string {
	const hex = "0123456789abcdef"
	last := []byte("000000000000")
	for i := len(last) - 1; i >= 0 && n > 0; i-- {
		last[i] = hex[n%16]
		n /= 16
	}
	return "00000000-0000-7000-8000-" + string(last)
}
