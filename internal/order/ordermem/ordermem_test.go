package ordermem_test

import (
	"context"
	"testing"

	"go.uber.org/goleak"

	"github.com/example/gomicro/internal/order"
	"github.com/example/gomicro/internal/order/ordermem"
	"github.com/example/gomicro/internal/order/ordertest"
)

// goleak turns "this package leaks a goroutine" from something you discover in
// production into something that fails the build. It is cheap enough to run everywhere.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// The in-memory store is held to the SAME contract as the Postgres adapter.
//
// That equivalence is what makes the fast tier trustworthy: when a business test uses
// this store and passes, it is passing against behaviour real Postgres has also been
// shown to have. Assertions only Postgres can make (SQLSTATE mapping, SKIP LOCKED,
// index usage) live in the adapter's own test, not in the shared contract.
func TestStoreContract(t *testing.T) {
	t.Parallel()

	ordertest.RunStoreContract(t, func(t *testing.T) ordertest.Harness {
		s := ordermem.New()
		return ordertest.Harness{
			Store:  s,
			Atomic: s,
			Events: func(context.Context) ([]order.Event, error) { return s.Events(), nil },
		}
	})
}

// Copy-on-write rollback must not corrupt state that a concurrent reader is holding.
func TestInTxRollbackLeavesStoreUsable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := ordermem.New()
	existing := ordertest.NewOrder(ordertest.WithID(ordertest.SeqID(1)))
	if err := s.Create(ctx, existing); err != nil {
		t.Fatalf("Create: %v", err)
	}

	boom := context.Canceled
	err := s.InTx(ctx, func(st order.Store, _ order.EventPublisher) error {
		if err := st.Create(ctx, ordertest.NewOrder(ordertest.WithID(ordertest.SeqID(2)))); err != nil {
			return err
		}
		return boom
	})
	if err == nil {
		t.Fatal("InTx returned nil, want the callback error")
	}

	if _, err := s.Get(ctx, ordertest.RefTenant, existing.ID); err != nil {
		t.Errorf("pre-existing order lost after rollback: %v", err)
	}
	page, err := s.List(ctx, ordertest.RefTenant, order.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Orders) != 1 {
		t.Errorf("got %d orders after rollback, want 1", len(page.Orders))
	}
}
