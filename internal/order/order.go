// Package order is the domain layer.
//
// It imports no generated protobuf code, no database driver, no gRPC, and no telemetry
// SDK. test/layout_test.go enforces that with go/packages, so the boundary is a
// compile-time property rather than a README promise.
//
// The practical payoff is the test tier: everything here runs in single-digit
// milliseconds with no Docker daemon, which is what keeps `go test ./...` useful.
package order

import (
	"fmt"
	"time"
)

// Status is the order lifecycle state.
type Status uint8

const (
	StatusUnspecified Status = iota
	StatusPending
	StatusConfirmed
	StatusShipped
	StatusCancelled
)

func (s Status) String() string {
	switch s {
	case StatusPending:
		return "PENDING"
	case StatusConfirmed:
		return "CONFIRMED"
	case StatusShipped:
		return "SHIPPED"
	case StatusCancelled:
		return "CANCELLED"
	default:
		return "UNSPECIFIED"
	}
}

// Valid reports whether s is a real state (i.e. not the zero value).
func (s Status) Valid() bool {
	return s >= StatusPending && s <= StatusCancelled
}

// allowedTransitions is the whole state machine, in one readable place.
var allowedTransitions = map[Status][]Status{
	StatusPending:   {StatusConfirmed, StatusCancelled},
	StatusConfirmed: {StatusShipped, StatusCancelled},
	StatusShipped:   {},
	StatusCancelled: {},
}

// CanTransitionTo reports whether s -> next is legal.
func (s Status) CanTransitionTo(next Status) bool {
	for _, allowed := range allowedTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// Item is one line on an order.
type Item struct {
	SKU       string
	Quantity  int32
	UnitPrice Money
}

// Validate reports whether the item is well-formed.
func (i Item) Validate() error {
	if i.SKU == "" {
		return fmt.Errorf("%w: sku is empty", ErrInvalidItem)
	}
	if len(i.SKU) > 64 {
		return fmt.Errorf("%w: sku exceeds 64 characters", ErrInvalidItem)
	}
	if i.Quantity <= 0 {
		return fmt.Errorf("%w: quantity %d must be positive", ErrInvalidItem, i.Quantity)
	}
	if err := i.UnitPrice.Validate(); err != nil {
		return fmt.Errorf("%w: unit price: %w", ErrInvalidItem, err)
	}
	if i.UnitPrice.IsNegative() {
		return fmt.Errorf("%w: unit price is negative", ErrInvalidItem)
	}
	return nil
}

// LineTotal is UnitPrice * Quantity.
func (i Item) LineTotal() (Money, error) {
	return i.UnitPrice.Mul(int64(i.Quantity))
}

// Order is the aggregate.
type Order struct {
	ID         string
	TenantID   string
	CustomerID string
	Status     Status
	Items      []Item
	Total      Money
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Validate reports whether the aggregate is internally consistent. It re-derives the
// total rather than trusting the stored one, so a store that round-trips an order
// incorrectly is caught by the shared store contract.
func (o Order) Validate() error {
	if o.TenantID == "" {
		return ErrMissingTenant
	}
	if o.CustomerID == "" {
		return fmt.Errorf("%w: customer id is empty", ErrInvalidItem)
	}
	if len(o.Items) == 0 {
		return ErrNoItems
	}
	for idx, item := range o.Items {
		if err := item.Validate(); err != nil {
			return fmt.Errorf("item %d: %w", idx, err)
		}
	}
	if !o.Status.Valid() {
		return fmt.Errorf("%w: status %d", ErrInvalidTransition, o.Status)
	}
	want, err := ComputeTotal(o.Items)
	if err != nil {
		return err
	}
	if want != o.Total {
		return fmt.Errorf("total %s does not match the sum of items %s", o.Total, want)
	}
	return nil
}

// Cancel moves the order to CANCELLED if that transition is legal.
func (o *Order) Cancel(at time.Time) error {
	if !o.Status.CanTransitionTo(StatusCancelled) {
		return fmt.Errorf("%w: %s -> CANCELLED", ErrInvalidTransition, o.Status)
	}
	o.Status = StatusCancelled
	o.UpdatedAt = at
	return nil
}

// ComputeTotal sums the line totals. All items must share a currency; mixing currencies
// on one order is rejected rather than silently summed.
func ComputeTotal(items []Item) (Money, error) {
	if len(items) == 0 {
		return Money{}, ErrNoItems
	}
	total := Zero(items[0].UnitPrice.CurrencyCode)
	for idx, item := range items {
		line, err := item.LineTotal()
		if err != nil {
			return Money{}, fmt.Errorf("item %d: %w", idx, err)
		}
		total, err = total.Add(line)
		if err != nil {
			return Money{}, fmt.Errorf("item %d: %w", idx, err)
		}
	}
	return total, nil
}
