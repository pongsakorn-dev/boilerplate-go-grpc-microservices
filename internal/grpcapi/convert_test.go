package grpcapi

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
	"github.com/example/gomicro/internal/order"
	"github.com/example/gomicro/internal/order/ordertest"
)

func TestOrderRoundTrip(t *testing.T) {
	t.Parallel()

	want := ordertest.NewOrder(
		ordertest.WithID("00000000-0000-7000-8000-00000000abcd"),
		ordertest.WithItems(
			ordertest.Item("SKU-1", 2, ordertest.USD(19, 990000000)),
			ordertest.Item("SKU-2", 7, ordertest.USD(0, 50000000)),
		),
	)

	got, err := fromProtoOrder(toProtoOrder(want))
	if err != nil {
		t.Fatalf("fromProtoOrder: %v", err)
	}

	// Timestamps go through timestamppb, which is nanosecond-precise, so this is an exact
	// comparison rather than an approximate one.
	if diff := cmp.Diff(want, got, cmp.Comparer(func(a, b time.Time) bool { return a.Equal(b) })); diff != "" {
		t.Errorf("order did not survive the proto round trip (-want +got):\n%s", diff)
	}
}

// mappedOrderFields is the explicit acknowledgement list for order.v1.Order.
//
// Every field is listed with the decision that was made about it. Adding a field to the
// .proto without adding it here fails the test below.
var mappedOrderFields = map[string]string{
	"order_id":    "mapped to Order.ID",
	"tenant_id":   "mapped to Order.TenantID",
	"customer_id": "mapped to Order.CustomerID",
	"status":      "mapped via toProtoStatus/fromProtoStatus",
	"items":       "mapped via toProtoItem/fromProtoItem",
	"total":       "mapped via toProtoMoney/fromProtoMoney",
	"created_at":  "mapped to Order.CreatedAt",
	"updated_at":  "mapped to Order.UpdatedAt",
}

// TestAllProtoFieldsAreAcknowledged catches the silent-omission bug.
//
// The realistic failure is not a wrong mapping -- that shows up immediately. It is an
// ADDED one: someone appends `discount_code` to the proto, regenerates, wires it into the
// handler, and forgets convert.go. The field then serializes as its zero value forever,
// every test passes, and the bug surfaces as "discounts silently do nothing in production".
//
// Nothing in the compiler can catch that, because protobuf fields are always optional at
// the language level. So the mapping is made explicit and this test enforces the list.
func TestAllProtoFieldsAreAcknowledged(t *testing.T) {
	t.Parallel()

	fields := (&orderv1.Order{}).ProtoReflect().Descriptor().Fields()
	if fields.Len() == 0 {
		t.Fatal("order.v1.Order has no fields -- this guard would silently pass forever")
	}

	seen := map[string]bool{}
	for i := range fields.Len() {
		name := string(fields.Get(i).Name())
		seen[name] = true

		if _, ok := mappedOrderFields[name]; !ok {
			t.Errorf("order.v1.Order.%s is not acknowledged in convert.go.\n\n"+
				"Map it in toProtoOrder/fromProtoOrder, then add it to mappedOrderFields with\n"+
				"a note. If it is deliberately not mapped, say so there -- the point is that a\n"+
				"human decided, rather than the field defaulting to zero forever.", name)
		}
	}

	for name := range mappedOrderFields {
		if !seen[name] {
			t.Errorf("mappedOrderFields lists %q, which no longer exists on order.v1.Order; "+
				"remove the stale entry", name)
		}
	}
}

// TestStatusMappingIsTotal proves no domain status silently collapses to UNSPECIFIED.
//
// A missing switch case in toProtoStatus does not fail to compile -- it falls through to
// the default and returns UNSPECIFIED. The order would then report "unknown status" to
// every client, which reads like a data problem rather than a mapping bug.
func TestStatusMappingIsTotal(t *testing.T) {
	t.Parallel()

	statuses := []order.Status{
		order.StatusPending,
		order.StatusConfirmed,
		order.StatusShipped,
		order.StatusCancelled,
	}

	seen := map[orderv1.OrderStatus]order.Status{}
	for _, s := range statuses {
		p := toProtoStatus(s)
		if p == orderv1.OrderStatus_ORDER_STATUS_UNSPECIFIED {
			t.Errorf("domain status %s maps to UNSPECIFIED -- a switch case is missing", s)
			continue
		}
		if prev, dup := seen[p]; dup {
			t.Errorf("domain statuses %s and %s both map to %s", prev, s, p)
		}
		seen[p] = s

		if back := fromProtoStatus(p); back != s {
			t.Errorf("round trip: %s -> %s -> %s", s, p, back)
		}
	}

	// The proto enum must not gain a value the domain cannot represent.
	values := orderv1.OrderStatus(0).Descriptor().Values()
	for i := range values.Len() {
		v := orderv1.OrderStatus(values.Get(i).Number())
		if v == orderv1.OrderStatus_ORDER_STATUS_UNSPECIFIED {
			continue
		}
		if fromProtoStatus(v) == order.StatusUnspecified {
			t.Errorf("proto status %s has no domain equivalent -- add it to fromProtoStatus", v)
		}
	}
}

func TestMoneyConversionRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   *orderv1.Money
	}{
		{"nil", nil},
		{"empty currency", &orderv1.Money{Units: 1}},
		{"lowercase currency", &orderv1.Money{CurrencyCode: "usd", Units: 1}},
		{"nanos out of range", &orderv1.Money{CurrencyCode: "USD", Nanos: 2_000_000_000}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := fromProtoMoney(tc.in); err == nil {
				t.Error("got nil error, want a validation failure")
			}
		})
	}
}

func TestMoneyRoundTrip(t *testing.T) {
	t.Parallel()

	want := &orderv1.Money{CurrencyCode: "USD", Units: 19, Nanos: 990000000}

	domain, err := fromProtoMoney(want)
	if err != nil {
		t.Fatalf("fromProtoMoney: %v", err)
	}
	if diff := cmp.Diff(want, toProtoMoney(domain), protocmp.Transform()); diff != "" {
		t.Errorf("money did not round-trip (-want +got):\n%s", diff)
	}
}
