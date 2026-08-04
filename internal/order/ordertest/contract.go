package ordertest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/example/gomicro/internal/order"
)

// tamperCursorID rewrites the id inside an opaque page token, leaving everything else --
// including the filter hash -- intact.
//
// It works on the decoded JSON rather than on order's unexported cursor struct, which is the
// point: this is what an ADVERSARY can do with nothing but a token they were legitimately
// given and five minutes. If the encoding changes, this helper fails loudly here rather than
// silently testing nothing.
func tamperCursorID(t *testing.T, token, newID string) string {
	t.Helper()

	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("the page token is not base64url, so this helper is out of date: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("the page token is not JSON, so this helper is out of date: %v", err)
	}
	if _, ok := fields["i"]; !ok {
		t.Fatalf("the page token has no %q field; this helper is out of date: %v", "i", fields)
	}
	fields["i"] = newID

	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Harness is one store implementation, ready to test.
type Harness struct {
	Store  order.Store
	Atomic order.Atomic

	// Events returns every event durably recorded so far. The in-memory store reads a
	// slice; the Postgres adapter selects from the outbox table. Exposing it as a
	// function is what lets ONE contract assert transactional behaviour against both.
	Events func(ctx context.Context) ([]order.Event, error)
}

// Factory builds a fresh, empty Harness for a single subtest.
type Factory func(t *testing.T) Harness

// timeCmp tolerates sub-microsecond differences. Postgres timestamptz has microsecond
// resolution, so a nanosecond-exact comparison would pass in memory and fail against the
// real database -- the divergence this contract exists to prevent.
var orderCmp = cmp.Options{cmpopts.EquateApproxTime(time.Microsecond)}

// RunStoreContract is the single behavioural contract every order.Store must satisfy.
//
// It runs unchanged against the in-memory store (no Docker, microseconds) and against
// real Postgres (testcontainers, seconds). That is the entire point: it turns "the fake
// behaves like the database" from an assumption into a tested property. A fake that
// nothing holds to a contract is just a second implementation of your bugs.
//
// Assertions that only real Postgres can make -- SQLSTATE mapping, SKIP LOCKED
// disjointness, index usage -- live in the adapter's own test file, not here.
func RunStoreContract(t *testing.T, newHarness Factory) {
	t.Helper()

	ctx := context.Background()

	t.Run("Create then Get round-trips every field", func(t *testing.T) {
		h := newHarness(t)
		want := NewOrder(WithID(SeqID(1)))

		if err := h.Store.Create(ctx, want); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := h.Store.Get(ctx, RefTenant, want.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if diff := cmp.Diff(want, got, orderCmp); diff != "" {
			t.Errorf("order did not round-trip (-want +got):\n%s", diff)
		}
	})

	t.Run("Get of an unknown id returns ErrNotFound", func(t *testing.T) {
		h := newHarness(t)
		_, err := h.Store.Get(ctx, RefTenant, SeqID(99))
		if !errors.Is(err, order.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("Create with a duplicate id returns ErrDuplicate", func(t *testing.T) {
		h := newHarness(t)
		o := NewOrder(WithID(SeqID(1)))
		if err := h.Store.Create(ctx, o); err != nil {
			t.Fatalf("first Create: %v", err)
		}
		err := h.Store.Create(ctx, o)
		if !errors.Is(err, order.ErrDuplicate) {
			t.Fatalf("got %v, want ErrDuplicate", err)
		}
	})

	// Tenant isolation is reported as NotFound, never PermissionDenied. Distinguishing
	// the two would turn Get into an existence oracle: a caller could enumerate ids and
	// learn which ones exist in OTHER tenants from the difference in error code.
	t.Run("an order in another tenant is indistinguishable from missing", func(t *testing.T) {
		h := newHarness(t)
		o := NewOrder(WithID(SeqID(1)), WithTenant(OtherTen))
		if err := h.Store.Create(ctx, o); err != nil {
			t.Fatalf("Create: %v", err)
		}
		_, err := h.Store.Get(ctx, RefTenant, o.ID)
		if !errors.Is(err, order.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound for a cross-tenant read", err)
		}
	})

	t.Run("writes without a tenant fail closed", func(t *testing.T) {
		h := newHarness(t)
		o := NewOrder(WithID(SeqID(1)), WithTenant(""))
		if err := h.Store.Create(ctx, o); !errors.Is(err, order.ErrMissingTenant) {
			t.Errorf("Create: got %v, want ErrMissingTenant", err)
		}
		if _, err := h.Store.Get(ctx, "", SeqID(1)); !errors.Is(err, order.ErrMissingTenant) {
			t.Errorf("Get: got %v, want ErrMissingTenant", err)
		}
		if _, err := h.Store.List(ctx, "", order.ListFilter{}); !errors.Is(err, order.ErrMissingTenant) {
			t.Errorf("List: got %v, want ErrMissingTenant", err)
		}
	})

	t.Run("Update persists changes", func(t *testing.T) {
		h := newHarness(t)
		o := NewOrder(WithID(SeqID(1)))
		if err := h.Store.Create(ctx, o); err != nil {
			t.Fatalf("Create: %v", err)
		}

		o.Status = order.StatusCancelled
		o.UpdatedAt = RefTime.Add(time.Hour)
		if err := h.Store.Update(ctx, o); err != nil {
			t.Fatalf("Update: %v", err)
		}

		got, err := h.Store.Get(ctx, RefTenant, o.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != order.StatusCancelled {
			t.Errorf("status = %v, want CANCELLED", got.Status)
		}
		if !got.UpdatedAt.Equal(o.UpdatedAt) {
			t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, o.UpdatedAt)
		}
	})

	t.Run("Update of an unknown id returns ErrNotFound", func(t *testing.T) {
		h := newHarness(t)
		err := h.Store.Update(ctx, NewOrder(WithID(SeqID(42))))
		if !errors.Is(err, order.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("List returns only the caller's tenant", func(t *testing.T) {
		h := newHarness(t)
		mustCreate(t, h, NewOrder(WithID(SeqID(1)), WithTenant(RefTenant)))
		mustCreate(t, h, NewOrder(WithID(SeqID(2)), WithTenant(OtherTen)))

		page, err := h.Store.List(ctx, RefTenant, order.ListFilter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page.Orders) != 1 || page.Orders[0].ID != SeqID(1) {
			t.Fatalf("got %d orders %v, want exactly the tenant-a order", len(page.Orders), ids(page.Orders))
		}
	})

	// Rows sharing a timestamp are the classic keyset-pagination bug: without a tiebreak
	// column the order is not total, so paging can skip or repeat rows.
	t.Run("List orders rows stably when timestamps collide", func(t *testing.T) {
		h := newHarness(t)
		for i := 1; i <= 5; i++ {
			mustCreate(t, h, NewOrder(WithID(SeqID(i)), WithCreatedAt(RefTime)))
		}
		first, err := h.Store.List(ctx, RefTenant, order.ListFilter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		second, err := h.Store.List(ctx, RefTenant, order.ListFilter{})
		if err != nil {
			t.Fatalf("List again: %v", err)
		}
		if diff := cmp.Diff(ids(first.Orders), ids(second.Orders)); diff != "" {
			t.Errorf("repeated List returned a different order (-first +second):\n%s", diff)
		}
	})

	t.Run("paging visits every row exactly once", func(t *testing.T) {
		h := newHarness(t)
		const total = 7
		for i := 1; i <= total; i++ {
			mustCreate(t, h, NewOrder(WithID(SeqID(i)), WithCreatedAt(RefTime.Add(time.Duration(i)*time.Second))))
		}

		seen := map[string]int{}
		filter := order.ListFilter{PageSize: 3}
		for pages := 0; ; pages++ {
			if pages > total+2 {
				t.Fatal("pagination did not terminate")
			}
			page, err := h.Store.List(ctx, RefTenant, filter)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			for _, o := range page.Orders {
				seen[o.ID]++
			}
			if page.NextPageToken == "" {
				break
			}
			filter.PageToken = page.NextPageToken
		}

		if len(seen) != total {
			t.Errorf("saw %d distinct orders, want %d", len(seen), total)
		}
		for id, n := range seen {
			if n != 1 {
				t.Errorf("order %s returned %d times, want exactly 1", id, n)
			}
		}
	})

	t.Run("List filters by status and customer", func(t *testing.T) {
		h := newHarness(t)
		mustCreate(t, h, NewOrder(WithID(SeqID(1)), WithStatus(order.StatusPending), WithCustomer("alice")))
		mustCreate(t, h, NewOrder(WithID(SeqID(2)), WithStatus(order.StatusCancelled), WithCustomer("alice")))
		mustCreate(t, h, NewOrder(WithID(SeqID(3)), WithStatus(order.StatusPending), WithCustomer("bob")))

		byStatus, err := h.Store.List(ctx, RefTenant, order.ListFilter{Status: order.StatusCancelled})
		if err != nil {
			t.Fatalf("List by status: %v", err)
		}
		if got := ids(byStatus.Orders); len(got) != 1 || got[0] != SeqID(2) {
			t.Errorf("by status = %v, want [%s]", got, SeqID(2))
		}

		byCustomer, err := h.Store.List(ctx, RefTenant, order.ListFilter{CustomerID: "bob"})
		if err != nil {
			t.Fatalf("List by customer: %v", err)
		}
		if got := ids(byCustomer.Orders); len(got) != 1 || got[0] != SeqID(3) {
			t.Errorf("by customer = %v, want [%s]", got, SeqID(3))
		}
	})

	// A token carries a hash of the filter it was issued for. Without that check, changing
	// the filter mid-pagination silently returns a wrong result set rather than an error.
	t.Run("a page token from a different filter is rejected", func(t *testing.T) {
		h := newHarness(t)
		for i := 1; i <= 3; i++ {
			mustCreate(t, h, NewOrder(WithID(SeqID(i)), WithCreatedAt(RefTime.Add(time.Duration(i)*time.Second))))
		}
		page, err := h.Store.List(ctx, RefTenant, order.ListFilter{PageSize: 1})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if page.NextPageToken == "" {
			t.Fatal("expected a next page token")
		}

		_, err = h.Store.List(ctx, RefTenant, order.ListFilter{
			PageSize:  1,
			PageToken: page.NextPageToken,
			Status:    order.StatusCancelled, // different filter, same token
		})
		if !errors.Is(err, order.ErrInvalidPageToken) {
			t.Fatalf("got %v, want ErrInvalidPageToken", err)
		}
	})

	t.Run("a malformed page token is rejected", func(t *testing.T) {
		h := newHarness(t)
		_, err := h.Store.List(ctx, RefTenant, order.ListFilter{PageToken: "!!!not-base64!!!"})
		if !errors.Is(err, order.ErrInvalidPageToken) {
			t.Fatalf("got %v, want ErrInvalidPageToken", err)
		}
	})

	// A token whose ID has been TAMPERED WITH but whose filter hash is still correct.
	//
	// This one belongs in the contract rather than in a unit test because it is the case
	// where the two implementations genuinely disagreed. The in-memory store compares ids as
	// strings and shrugs; Postgres compares against a uuid column and raises `invalid input
	// syntax for type uuid`, which surfaced as a 500 for input the caller supplied.
	//
	// The filter hash is not a defence here, and that is the subtle part. It covers tenant,
	// status and customer -- the fields that change the result set -- and deliberately not
	// the id. So a caller decodes a token they were legitimately given, edits the id,
	// re-encodes, and the hash still matches.
	t.Run("a page token with a tampered id is rejected", func(t *testing.T) {
		h := newHarness(t)
		for i := 1; i <= 3; i++ {
			mustCreate(t, h, NewOrder(WithID(SeqID(i)), WithCreatedAt(RefTime.Add(time.Duration(i)*time.Second))))
		}
		page, err := h.Store.List(ctx, RefTenant, order.ListFilter{PageSize: 1})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if page.NextPageToken == "" {
			t.Fatal("expected a next page token")
		}

		tampered := tamperCursorID(t, page.NextPageToken, "'; DROP TABLE orders; --")

		// Same filter as the token was issued for, so the hash check passes and the id check
		// is the only thing standing between this and the query.
		_, err = h.Store.List(ctx, RefTenant, order.ListFilter{PageSize: 1, PageToken: tampered})
		if !errors.Is(err, order.ErrInvalidPageToken) {
			t.Fatalf("got %v, want ErrInvalidPageToken.\n\n"+
				"A tampered id reached the store. Against Postgres that is a uuid parse error "+
				"from the driver, which nothing classifies, so the caller gets a 500 for input "+
				"they sent -- and it should have been an InvalidArgument.", err)
		}
	})

	// The outbox guarantee, stated as a test: the order and its event commit together or
	// not at all. If these could commit independently you would get orders with no event
	// (a silently lost integration) or events for orders that never existed.
	t.Run("a rolled back transaction writes neither the order nor the event", func(t *testing.T) {
		h := newHarness(t)
		o := NewOrder(WithID(SeqID(1)))
		boom := errors.New("business rule failed")

		err := h.Atomic.InTx(ctx, func(st order.Store, pub order.EventPublisher) error {
			if err := st.Create(ctx, o); err != nil {
				return err
			}
			if err := pub.Publish(ctx, eventFor(o, order.EventOrderCreated)); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("InTx returned %v, want the callback error", err)
		}

		if _, err := h.Store.Get(ctx, RefTenant, o.ID); !errors.Is(err, order.ErrNotFound) {
			t.Errorf("order survived a rolled back transaction (got %v)", err)
		}
		events, err := h.Events(ctx)
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		if len(events) != 0 {
			t.Errorf("got %d events after rollback, want 0", len(events))
		}
	})

	t.Run("a committed transaction writes both the order and the event", func(t *testing.T) {
		h := newHarness(t)
		o := NewOrder(WithID(SeqID(1)))

		err := h.Atomic.InTx(ctx, func(st order.Store, pub order.EventPublisher) error {
			if err := st.Create(ctx, o); err != nil {
				return err
			}
			return pub.Publish(ctx, eventFor(o, order.EventOrderCreated))
		})
		if err != nil {
			t.Fatalf("InTx: %v", err)
		}

		if _, err := h.Store.Get(ctx, RefTenant, o.ID); err != nil {
			t.Errorf("order missing after commit: %v", err)
		}
		events, err := h.Events(ctx)
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		if events[0].Type != order.EventOrderCreated || events[0].AggregateID != o.ID {
			t.Errorf("event = %+v, want OrderCreated for %s", events[0], o.ID)
		}
	})
}

func mustCreate(t *testing.T, h Harness, o order.Order) {
	t.Helper()
	if err := h.Store.Create(context.Background(), o); err != nil {
		t.Fatalf("Create %s: %v", o.ID, err)
	}
}

func eventFor(o order.Order, typ string) order.Event {
	return order.Event{
		Type:        typ,
		AggregateID: o.ID,
		TenantID:    o.TenantID,
		OccurredAt:  o.CreatedAt,
		Order:       o,
	}
}

func ids(orders []order.Order) []string {
	out := make([]string, len(orders))
	for i, o := range orders {
		out[i] = o.ID
	}
	return out
}
