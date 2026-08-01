// Package test holds cross-cutting guards. No business logic is tested here.
package test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/google/go-cmp/cmp"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
	"github.com/example/gomicro/internal/testutil"
)

// TestQuickstart executes the README's promise instead of asserting it.
//
// A template's single most important property is that a stranger can clone it and see it
// work in minutes. That property normally rots silently: the README keeps claiming
// something the fifteenth commit quietly broke. This test performs the documented first
// run -- boot with no database, list the seeded orders, create one, cancel it, check
// health, check reflection -- so the claim is regression-guarded like any other behaviour.
//
// It needs no Docker, no network, and no configuration.
func TestQuickstart(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := testutil.NewTestServer(t)
	client := orderv1.NewOrderServiceClient(conn)

	t.Run("the service reports SERVING", func(t *testing.T) {
		// This is the exact call Kubernetes' native grpc: probe makes. An empty service
		// name means "overall status".
		resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
		if err != nil {
			t.Fatalf("Health/Check: %v", err)
		}
		if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
			t.Errorf("status = %v, want SERVING", resp.GetStatus())
		}
	})

	t.Run("ListOrders returns the seeded rows", func(t *testing.T) {
		resp, err := client.ListOrders(ctx, &orderv1.ListOrdersRequest{})
		if err != nil {
			t.Fatalf("ListOrders: %v", err)
		}
		if len(resp.GetOrders()) != 3 {
			t.Fatalf("got %d seeded orders, want 3", len(resp.GetOrders()))
		}
		// An empty first response looks broken even when it is correct, which is why the
		// memory driver seeds at all.
		if resp.GetOrders()[0].GetTotal().GetCurrencyCode() != "USD" {
			t.Errorf("currency = %q, want USD", resp.GetOrders()[0].GetTotal().GetCurrencyCode())
		}
	})

	t.Run("create, read back, then cancel", func(t *testing.T) {
		created, err := client.CreateOrder(ctx, &orderv1.CreateOrderRequest{
			CustomerId: "customer-quickstart",
			Items: []*orderv1.OrderItem{{
				Sku:       "WIDGET-BLUE",
				Quantity:  3,
				UnitPrice: &orderv1.Money{CurrencyCode: "USD", Units: 10, Nanos: 500000000},
			}},
		})
		if err != nil {
			t.Fatalf("CreateOrder: %v", err)
		}

		// 10.50 * 3 = 31.50, computed by the server. A client that sends its own total
		// would be trusted by a lesser design.
		gotTotal := created.GetOrder().GetTotal()
		wantTotal := &orderv1.Money{CurrencyCode: "USD", Units: 31, Nanos: 500000000}
		if diff := cmp.Diff(wantTotal, gotTotal, protocmp.Transform()); diff != "" {
			t.Errorf("total mismatch (-want +got):\n%s", diff)
		}
		if created.GetOrder().GetStatus() != orderv1.OrderStatus_ORDER_STATUS_PENDING {
			t.Errorf("status = %v, want PENDING", created.GetOrder().GetStatus())
		}

		id := created.GetOrder().GetOrderId()

		got, err := client.GetOrder(ctx, &orderv1.GetOrderRequest{OrderId: id})
		if err != nil {
			t.Fatalf("GetOrder: %v", err)
		}
		if diff := cmp.Diff(created.GetOrder(), got.GetOrder(), protocmp.Transform()); diff != "" {
			t.Errorf("GetOrder did not return what CreateOrder produced (-created +got):\n%s", diff)
		}

		cancelled, err := client.CancelOrder(ctx, &orderv1.CancelOrderRequest{OrderId: id})
		if err != nil {
			t.Fatalf("CancelOrder: %v", err)
		}
		if cancelled.GetOrder().GetStatus() != orderv1.OrderStatus_ORDER_STATUS_CANCELLED {
			t.Errorf("status = %v, want CANCELLED", cancelled.GetOrder().GetStatus())
		}

		// Cancelling twice is an illegal transition, not a silent success.
		_, err = client.CancelOrder(ctx, &orderv1.CancelOrderRequest{OrderId: id})
		if got, want := status.Code(err), codes.FailedPrecondition; got != want {
			t.Errorf("second cancel: code = %v, want %v (err=%v)", got, want, err)
		}
	})

	t.Run("a missing order is NotFound, not Internal", func(t *testing.T) {
		_, err := client.GetOrder(ctx, &orderv1.GetOrderRequest{
			OrderId: "00000000-0000-7000-8000-00000000dead",
		})
		if got, want := status.Code(err), codes.NotFound; got != want {
			t.Fatalf("code = %v, want %v (err=%v)", got, want, err)
		}
	})
}
