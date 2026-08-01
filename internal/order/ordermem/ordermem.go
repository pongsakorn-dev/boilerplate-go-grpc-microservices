// Package ordermem is an in-memory order.Store and order.Atomic.
//
// It is NOT a test double that lives in a _test.go file. It is a real, supported
// implementation that backs STORE_DRIVER=memory, which is what lets a fresh clone run
// `go run ./cmd/orderd` with no database, no Docker and no configuration.
//
// The same implementation is what the unit test tier runs against. That is deliberate:
// because ordertest.RunStoreContract is executed against BOTH this and the GORM/Postgres
// adapter, the fast in-memory tier is provably equivalent to the real one on every
// behaviour the contract covers. A hand-written fake that nothing holds to a contract is
// just a second implementation of your bugs.
//
// It deliberately does not import "testing", so nothing here drags the testing package
// into a production binary.
package ordermem

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/example/gomicro/internal/order"
)

// Store is a goroutine-safe in-memory implementation of order.Store, order.Atomic and
// order.EventPublisher.
type Store struct {
	mu   sync.Mutex
	data *state
}

type state struct {
	// keyed by tenantID + "\x00" + orderID, so a lookup can never cross tenants even if
	// a caller forgets to filter.
	orders map[string]order.Order
	events []order.Event
}

func (s *state) clone() *state {
	out := &state{
		orders: make(map[string]order.Order, len(s.orders)),
		events: append([]order.Event(nil), s.events...),
	}
	for k, v := range s.orders {
		out.orders[k] = v
	}
	return out
}

// New returns an empty store.
func New() *Store {
	return &Store{data: &state{orders: make(map[string]order.Order)}}
}

func key(tenantID, orderID string) string { return tenantID + "\x00" + orderID }

// view implements order.Store and order.EventPublisher over a state WITHOUT locking.
// The caller (Store's exported methods, or InTx) owns the lock. Keeping the locking out
// of the view is what prevents InTx -> Create from deadlocking on a non-reentrant mutex.
type view struct{ d *state }

func (v view) Create(_ context.Context, o order.Order) error {
	if o.TenantID == "" {
		return order.ErrMissingTenant
	}
	k := key(o.TenantID, o.ID)
	if _, exists := v.d.orders[k]; exists {
		return fmt.Errorf("%w: id %s", order.ErrDuplicate, o.ID)
	}
	v.d.orders[k] = o
	return nil
}

func (v view) Get(_ context.Context, tenantID, orderID string) (order.Order, error) {
	if tenantID == "" {
		return order.Order{}, order.ErrMissingTenant
	}
	o, ok := v.d.orders[key(tenantID, orderID)]
	if !ok {
		return order.Order{}, fmt.Errorf("%w: id %s", order.ErrNotFound, orderID)
	}
	return o, nil
}

func (v view) Update(_ context.Context, o order.Order) error {
	if o.TenantID == "" {
		return order.ErrMissingTenant
	}
	k := key(o.TenantID, o.ID)
	if _, ok := v.d.orders[k]; !ok {
		return fmt.Errorf("%w: id %s", order.ErrNotFound, o.ID)
	}
	v.d.orders[k] = o
	return nil
}

func (v view) List(_ context.Context, tenantID string, f order.ListFilter) (order.Page, error) {
	if tenantID == "" {
		return order.Page{}, order.ErrMissingTenant
	}

	after, afterID, hasCursor, err := order.DecodePageToken(tenantID, f)
	if err != nil {
		return order.Page{}, err
	}

	var matched []order.Order
	for _, o := range v.d.orders {
		if o.TenantID != tenantID {
			continue
		}
		if f.Status != order.StatusUnspecified && o.Status != f.Status {
			continue
		}
		if f.CustomerID != "" && o.CustomerID != f.CustomerID {
			continue
		}
		matched = append(matched, o)
	}

	// (CreatedAt, ID) is a TOTAL order. Sorting by CreatedAt alone would leave rows that
	// share a timestamp in map iteration order, and keyset pagination over a non-total
	// order silently skips and repeats rows.
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.Before(matched[j].CreatedAt)
		}
		return matched[i].ID < matched[j].ID
	})

	if hasCursor {
		idx := 0
		for idx < len(matched) {
			o := matched[idx]
			if o.CreatedAt.After(after) || (o.CreatedAt.Equal(after) && o.ID > afterID) {
				break
			}
			idx++
		}
		matched = matched[idx:]
	}

	size := int(order.ClampPageSize(f.PageSize))
	page := order.Page{}
	if len(matched) > size {
		page.Orders = matched[:size]
		token, err := order.EncodePageToken(tenantID, f, page.Orders[size-1])
		if err != nil {
			return order.Page{}, err
		}
		page.NextPageToken = token
	} else {
		page.Orders = matched
	}
	return page, nil
}

func (v view) Publish(_ context.Context, e order.Event) error {
	v.d.events = append(v.d.events, e)
	return nil
}

// Exported methods lock and delegate.

func (s *Store) Create(ctx context.Context, o order.Order) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return view{s.data}.Create(ctx, o)
}

func (s *Store) Get(ctx context.Context, tenantID, orderID string) (order.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return view{s.data}.Get(ctx, tenantID, orderID)
}

func (s *Store) Update(ctx context.Context, o order.Order) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return view{s.data}.Update(ctx, o)
}

func (s *Store) List(ctx context.Context, tenantID string, f order.ListFilter) (order.Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return view{s.data}.List(ctx, tenantID, f)
}

func (s *Store) Publish(ctx context.Context, e order.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return view{s.data}.Publish(ctx, e)
}

// InTx runs fn against a private copy of the state and swaps it in only on success.
//
// Copy-on-write gives real rollback semantics: a callback that fails leaves neither the
// order nor its outbox event behind. The shared store contract asserts exactly that
// against this implementation AND against Postgres, which is what makes the in-memory
// tier trustworthy rather than decorative.
func (s *Store) InTx(ctx context.Context, fn func(order.Store, order.EventPublisher) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	staged := s.data.clone()
	v := view{staged}
	if err := fn(v, v); err != nil {
		return err // staged is discarded: nothing committed
	}
	s.data = staged
	return nil
}

// Events returns a copy of everything published so far. Used by tests to assert on
// emitted events without a broker.
func (s *Store) Events() []order.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]order.Event(nil), s.data.events...)
}

// Reset empties the store.
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = &state{orders: make(map[string]order.Order)}
}

// Compile-time proof that the in-memory implementation satisfies every port. Without
// these, a signature drift would only surface wherever the store happens to be wired.
var (
	_ order.Store          = (*Store)(nil)
	_ order.Atomic         = (*Store)(nil)
	_ order.EventPublisher = (*Store)(nil)
)
