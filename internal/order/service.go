package order

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Service holds the business rules. It knows nothing about gRPC, HTTP, SQL, or
// protobuf -- it is handed ports and calls them.
type Service struct {
	store  Store
	atomic Atomic

	// now and newID are injected rather than called directly so tests can pin them.
	//
	// Note there is deliberately no Clock abstraction package: testing/synctest (GA in
	// Go 1.25) makes time deterministic inside a bubble for anything that actually
	// sleeps, and for the two values that need pinning here a function field is enough.
	// Shipping a Clock interface would teach readers to inject an abstraction the
	// standard library has made unnecessary.
	now   func() time.Time
	newID func() (string, error)
}

// Option customises a Service.
type Option func(*Service)

// WithClock pins the clock. Tests use it to assert exact timestamps.
func WithClock(fn func() time.Time) Option {
	return func(s *Service) { s.now = fn }
}

// WithIDGenerator pins id generation. Tests use it to get predictable ids.
func WithIDGenerator(fn func() (string, error)) Option {
	return func(s *Service) { s.newID = fn }
}

// NewService wires the domain service.
func NewService(store Store, atomic Atomic, opts ...Option) *Service {
	s := &Service{
		store:  store,
		atomic: atomic,
		now:    time.Now,
		newID:  newUUIDv7,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// newUUIDv7 generates a time-ordered identifier.
//
// Version 7 rather than 4 because the value is a primary key: v4 is uniformly random, so
// every insert lands in a random B-tree page and the index write amplifies badly once the
// table exceeds memory. v7 is time-prefixed, so inserts stay at the right edge of the tree.
//
// The trade-off is that a v7 id leaks its creation time to anyone holding it. For order
// ids that is acceptable; for anything where creation time is sensitive, use v4.
//
// Ids are generated in the application, not by the database, so the id exists before the
// INSERT -- which is what lets the outbox row reference it inside the same transaction.
func newUUIDv7() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate order id: %w", err)
	}
	return id.String(), nil
}

// CreateCommand is the input to Create.
//
// There is no idempotency key here on purpose, and there is no idempotency ANYWHERE in this
// repository -- it was cut. See docs/adr/0002-what-was-cut.md.
//
// This used to say the mechanism "is applied as a wrapper at the API boundary
// (internal/platform/idempotency)", present tense, naming a package that has never existed.
// The reasoning in that sentence is still the right reasoning -- idempotency is a
// transport-level concern about retried requests rather than a business rule, so it belongs
// at the boundary and not threaded through the domain. It describes where a fork should put
// it, not where this repository keeps it.
//
// CreateOrderRequest does carry an idempotency_key field. It is validated and ignored; the
// note on it in the .proto says so.
type CreateCommand struct {
	TenantID   string
	CustomerID string
	Items      []Item
}

// Create validates and persists a new order, publishing OrderCreated in the same
// transaction as the insert.
func (s *Service) Create(ctx context.Context, cmd CreateCommand) (Order, error) {
	if cmd.TenantID == "" {
		return Order{}, ErrMissingTenant
	}
	if len(cmd.Items) == 0 {
		return Order{}, ErrNoItems
	}

	total, err := ComputeTotal(cmd.Items)
	if err != nil {
		return Order{}, err
	}

	id, err := s.newID()
	if err != nil {
		return Order{}, err
	}

	now := s.now().UTC()
	o := Order{
		ID:         id,
		TenantID:   cmd.TenantID,
		CustomerID: cmd.CustomerID,
		Status:     StatusPending,
		Items:      cmd.Items,
		Total:      total,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := o.Validate(); err != nil {
		return Order{}, err
	}

	// The insert and the event are one transaction. If the process dies between them,
	// neither happened -- which is the entire reason the outbox exists.
	err = s.atomic.InTx(ctx, func(st Store, pub EventPublisher) error {
		if err := st.Create(ctx, o); err != nil {
			return err
		}
		return pub.Publish(ctx, Event{
			Type:        EventOrderCreated,
			AggregateID: o.ID,
			TenantID:    o.TenantID,
			OccurredAt:  now,
			Order:       o,
		})
	})
	if err != nil {
		return Order{}, err
	}
	return o, nil
}

// Get returns one order within the tenant.
func (s *Service) Get(ctx context.Context, tenantID, orderID string) (Order, error) {
	if tenantID == "" {
		return Order{}, ErrMissingTenant
	}
	return s.store.Get(ctx, tenantID, orderID)
}

// List returns a page of orders within the tenant.
func (s *Service) List(ctx context.Context, tenantID string, f ListFilter) (Page, error) {
	if tenantID == "" {
		return Page{}, ErrMissingTenant
	}
	f.PageSize = ClampPageSize(f.PageSize)
	return s.store.List(ctx, tenantID, f)
}

// Cancel transitions an order to CANCELLED, publishing OrderCancelled in the same
// transaction as the update.
//
// The read, the transition check, and the write all happen inside one transaction. Doing
// the read outside would be a lost-update race: two concurrent cancels would both observe
// PENDING and both succeed.
func (s *Service) Cancel(ctx context.Context, tenantID, orderID string) (Order, error) {
	if tenantID == "" {
		return Order{}, ErrMissingTenant
	}

	var out Order
	err := s.atomic.InTx(ctx, func(st Store, pub EventPublisher) error {
		o, err := st.Get(ctx, tenantID, orderID)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		if err := o.Cancel(now); err != nil {
			return err
		}
		if err := st.Update(ctx, o); err != nil {
			return err
		}
		out = o
		return pub.Publish(ctx, Event{
			Type:        EventOrderCancelled,
			AggregateID: o.ID,
			TenantID:    o.TenantID,
			OccurredAt:  now,
			Order:       o,
		})
	})
	if err != nil {
		return Order{}, err
	}
	return out, nil
}
