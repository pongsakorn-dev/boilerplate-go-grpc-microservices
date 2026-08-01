package order_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/example/gomicro/internal/order"
	"github.com/example/gomicro/internal/order/ordermem"
	"github.com/example/gomicro/internal/order/ordertest"
)

// newService builds a Service with pinned time and ids.
//
// Pinning both is what makes every assertion below an exact equality rather than a
// "roughly now" check. Tests that assert on time.Now() are the ones that fail at 00:00 UTC
// and on a slow CI runner.
func newService(t *testing.T) (*order.Service, *ordermem.Store) {
	t.Helper()

	store := ordermem.New()
	var seq int
	svc := order.NewService(store, store,
		order.WithClock(func() time.Time { return ordertest.RefTime }),
		order.WithIDGenerator(func() (string, error) {
			seq++
			return ordertest.SeqID(seq), nil
		}),
	)
	return svc, store
}

func TestServiceCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("persists the order and computes the total", func(t *testing.T) {
		t.Parallel()
		svc, store := newService(t)

		got, err := svc.Create(ctx, order.CreateCommand{
			TenantID:   ordertest.RefTenant,
			CustomerID: "customer-1",
			Items: []order.Item{
				ordertest.Item("SKU-1", 2, ordertest.USD(19, 990000000)),
				ordertest.Item("SKU-2", 1, ordertest.USD(5, 0)),
			},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		// 19.99*2 + 5.00 = 44.98
		if got.Total.Units != 44 || got.Total.Nanos != 980000000 {
			t.Errorf("total = %s, want 44.98 USD", got.Total)
		}
		if got.Status != order.StatusPending {
			t.Errorf("status = %s, want PENDING", got.Status)
		}
		if !got.CreatedAt.Equal(ordertest.RefTime) {
			t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, ordertest.RefTime)
		}

		stored, err := store.Get(ctx, ordertest.RefTenant, got.ID)
		if err != nil {
			t.Fatalf("order was not persisted: %v", err)
		}
		if stored.ID != got.ID {
			t.Errorf("stored id %s != returned id %s", stored.ID, got.ID)
		}
	})

	t.Run("publishes exactly one OrderCreated event", func(t *testing.T) {
		t.Parallel()
		svc, store := newService(t)

		got, err := svc.Create(ctx, order.CreateCommand{
			TenantID:   ordertest.RefTenant,
			CustomerID: "customer-1",
			Items:      []order.Item{ordertest.Item("SKU-1", 1, ordertest.USD(1, 0))},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		events := store.Events()
		if len(events) != 1 {
			t.Fatalf("got %d events, want exactly 1", len(events))
		}
		if events[0].Type != order.EventOrderCreated {
			t.Errorf("event type = %q, want %q", events[0].Type, order.EventOrderCreated)
		}
		if events[0].AggregateID != got.ID {
			t.Errorf("event aggregate = %q, want %q", events[0].AggregateID, got.ID)
		}
		if events[0].TenantID != ordertest.RefTenant {
			t.Errorf("event tenant = %q, want %q", events[0].TenantID, ordertest.RefTenant)
		}
	})

	t.Run("rejects invalid input before touching the store", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name string
			cmd  order.CreateCommand
			want error
		}{
			{
				name: "no tenant",
				cmd:  order.CreateCommand{CustomerID: "c", Items: []order.Item{ordertest.Item("S", 1, ordertest.USD(1, 0))}},
				want: order.ErrMissingTenant,
			},
			{
				name: "no items",
				cmd:  order.CreateCommand{TenantID: ordertest.RefTenant, CustomerID: "c"},
				want: order.ErrNoItems,
			},
			{
				name: "zero quantity",
				cmd: order.CreateCommand{TenantID: ordertest.RefTenant, CustomerID: "c",
					Items: []order.Item{ordertest.Item("SKU-1", 0, ordertest.USD(1, 0))}},
				want: order.ErrInvalidItem,
			},
			{
				name: "negative quantity",
				cmd: order.CreateCommand{TenantID: ordertest.RefTenant, CustomerID: "c",
					Items: []order.Item{ordertest.Item("SKU-1", -3, ordertest.USD(1, 0))}},
				want: order.ErrInvalidItem,
			},
			{
				name: "blank sku",
				cmd: order.CreateCommand{TenantID: ordertest.RefTenant, CustomerID: "c",
					Items: []order.Item{ordertest.Item("", 1, ordertest.USD(1, 0))}},
				want: order.ErrInvalidItem,
			},
			{
				name: "mixed currencies on one order",
				cmd: order.CreateCommand{TenantID: ordertest.RefTenant, CustomerID: "c",
					Items: []order.Item{
						ordertest.Item("SKU-1", 1, ordertest.USD(1, 0)),
						ordertest.Item("SKU-2", 1, mustMoney(t, "EUR", 1, 0)),
					}},
				want: order.ErrCurrencyMismatch,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				svc, store := newService(t)

				_, err := svc.Create(ctx, tc.cmd)
				if !errors.Is(err, tc.want) {
					t.Fatalf("got %v, want %v", err, tc.want)
				}
				if events := store.Events(); len(events) != 0 {
					t.Errorf("published %d events for a rejected command, want 0", len(events))
				}
			})
		}
	})

	// If the event write fails, the order must not exist. This is the outbox invariant
	// expressed at the service level rather than the store level.
	t.Run("a failing event write rolls the order back", func(t *testing.T) {
		t.Parallel()

		store := ordermem.New()
		boom := errors.New("outbox unavailable")
		svc := order.NewService(store, failingPublisher{inner: store, err: boom},
			order.WithClock(func() time.Time { return ordertest.RefTime }),
			order.WithIDGenerator(func() (string, error) { return ordertest.SeqID(1), nil }),
		)

		_, err := svc.Create(ctx, order.CreateCommand{
			TenantID:   ordertest.RefTenant,
			CustomerID: "customer-1",
			Items:      []order.Item{ordertest.Item("SKU-1", 1, ordertest.USD(1, 0))},
		})
		if !errors.Is(err, boom) {
			t.Fatalf("got %v, want the publisher error", err)
		}

		if _, err := store.Get(ctx, ordertest.RefTenant, ordertest.SeqID(1)); !errors.Is(err, order.ErrNotFound) {
			t.Errorf("order survived a failed event write (got %v)", err)
		}
	})
}

func TestServiceCancel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("cancels a pending order and publishes the event", func(t *testing.T) {
		t.Parallel()
		svc, store := newService(t)

		created, err := svc.Create(ctx, order.CreateCommand{
			TenantID: ordertest.RefTenant, CustomerID: "c",
			Items: []order.Item{ordertest.Item("SKU-1", 1, ordertest.USD(1, 0))},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		got, err := svc.Cancel(ctx, ordertest.RefTenant, created.ID)
		if err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if got.Status != order.StatusCancelled {
			t.Errorf("status = %s, want CANCELLED", got.Status)
		}

		events := store.Events()
		if len(events) != 2 {
			t.Fatalf("got %d events, want 2 (created + cancelled)", len(events))
		}
		if events[1].Type != order.EventOrderCancelled {
			t.Errorf("second event = %q, want %q", events[1].Type, order.EventOrderCancelled)
		}
	})

	t.Run("refuses an illegal transition", func(t *testing.T) {
		t.Parallel()
		svc, store := newService(t)

		shipped := ordertest.NewOrder(
			ordertest.WithID(ordertest.SeqID(7)),
			ordertest.WithStatus(order.StatusShipped),
		)
		if err := store.Create(ctx, shipped); err != nil {
			t.Fatalf("seed: %v", err)
		}

		_, err := svc.Cancel(ctx, ordertest.RefTenant, shipped.ID)
		if !errors.Is(err, order.ErrInvalidTransition) {
			t.Fatalf("got %v, want ErrInvalidTransition", err)
		}
		if events := store.Events(); len(events) != 0 {
			t.Errorf("published %d events for a refused transition, want 0", len(events))
		}
	})

	t.Run("cancelling an unknown order returns ErrNotFound", func(t *testing.T) {
		t.Parallel()
		svc, _ := newService(t)

		_, err := svc.Cancel(ctx, ordertest.RefTenant, ordertest.SeqID(99))
		if !errors.Is(err, order.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("cannot cancel another tenant's order", func(t *testing.T) {
		t.Parallel()
		svc, store := newService(t)

		other := ordertest.NewOrder(ordertest.WithID(ordertest.SeqID(1)), ordertest.WithTenant(ordertest.OtherTen))
		if err := store.Create(ctx, other); err != nil {
			t.Fatalf("seed: %v", err)
		}

		_, err := svc.Cancel(ctx, ordertest.RefTenant, other.ID)
		if !errors.Is(err, order.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})
}

func TestServiceListClampsPageSize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	svc, store := newService(t)
	for i := 1; i <= 3; i++ {
		o := ordertest.NewOrder(
			ordertest.WithID(ordertest.SeqID(i)),
			ordertest.WithCreatedAt(ordertest.RefTime.Add(time.Duration(i)*time.Second)),
		)
		if err := store.Create(ctx, o); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// A caller asking for more than MaxPageSize gets MaxPageSize, not an error and not
	// an unbounded scan.
	page, err := svc.List(ctx, ordertest.RefTenant, order.ListFilter{PageSize: 99999})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Orders) != 3 {
		t.Errorf("got %d orders, want 3", len(page.Orders))
	}

	if _, err := svc.List(ctx, "", order.ListFilter{}); !errors.Is(err, order.ErrMissingTenant) {
		t.Errorf("List without a tenant: got %v, want ErrMissingTenant", err)
	}
}

// failingPublisher wraps a real Atomic but makes Publish fail, so the test can prove the
// business write is rolled back with it.
type failingPublisher struct {
	inner order.Atomic
	err   error
}

func (f failingPublisher) InTx(ctx context.Context, fn func(order.Store, order.EventPublisher) error) error {
	return f.inner.InTx(ctx, func(st order.Store, _ order.EventPublisher) error {
		return fn(st, brokenPublisher{f.err})
	})
}

type brokenPublisher struct{ err error }

func (b brokenPublisher) Publish(context.Context, order.Event) error { return b.err }

func mustMoney(t *testing.T, code string, units int64, nanos int32) order.Money {
	t.Helper()
	m, err := order.NewMoney(code, units, nanos)
	if err != nil {
		t.Fatalf("NewMoney: %v", err)
	}
	return m
}
