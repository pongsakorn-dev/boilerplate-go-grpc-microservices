// Package grpcapi is the proto boundary.
//
// It is the ONLY package that knows about both the generated protobuf types and the
// domain types. Everything above it speaks proto; everything below it speaks domain.
//
// The mapping code here is the price of that separation, and it is a price worth paying:
// it means a proto field rename is a change to one file rather than a database migration,
// and it means the domain and its tests never import protobuf at all.
package grpcapi

import (
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
	"github.com/example/gomicro/internal/order"
)

func toProtoMoney(m order.Money) *orderv1.Money {
	return &orderv1.Money{
		CurrencyCode: m.CurrencyCode,
		Units:        m.Units,
		Nanos:        m.Nanos,
	}
}

func fromProtoMoney(m *orderv1.Money) (order.Money, error) {
	if m == nil {
		return order.Money{}, fmt.Errorf("%w: money is required", order.ErrInvalidItem)
	}
	return order.NewMoney(m.GetCurrencyCode(), m.GetUnits(), m.GetNanos())
}

func toProtoStatus(s order.Status) orderv1.OrderStatus {
	switch s {
	case order.StatusPending:
		return orderv1.OrderStatus_ORDER_STATUS_PENDING
	case order.StatusConfirmed:
		return orderv1.OrderStatus_ORDER_STATUS_CONFIRMED
	case order.StatusShipped:
		return orderv1.OrderStatus_ORDER_STATUS_SHIPPED
	case order.StatusCancelled:
		return orderv1.OrderStatus_ORDER_STATUS_CANCELLED
	default:
		return orderv1.OrderStatus_ORDER_STATUS_UNSPECIFIED
	}
}

func fromProtoStatus(s orderv1.OrderStatus) order.Status {
	switch s {
	case orderv1.OrderStatus_ORDER_STATUS_PENDING:
		return order.StatusPending
	case orderv1.OrderStatus_ORDER_STATUS_CONFIRMED:
		return order.StatusConfirmed
	case orderv1.OrderStatus_ORDER_STATUS_SHIPPED:
		return order.StatusShipped
	case orderv1.OrderStatus_ORDER_STATUS_CANCELLED:
		return order.StatusCancelled
	default:
		return order.StatusUnspecified
	}
}

func toProtoItem(i order.Item) *orderv1.OrderItem {
	return &orderv1.OrderItem{
		Sku:       i.SKU,
		Quantity:  i.Quantity,
		UnitPrice: toProtoMoney(i.UnitPrice),
	}
}

func fromProtoItem(i *orderv1.OrderItem) (order.Item, error) {
	if i == nil {
		return order.Item{}, fmt.Errorf("%w: item is nil", order.ErrInvalidItem)
	}
	price, err := fromProtoMoney(i.GetUnitPrice())
	if err != nil {
		return order.Item{}, err
	}
	return order.Item{SKU: i.GetSku(), Quantity: i.GetQuantity(), UnitPrice: price}, nil
}

func fromProtoItems(in []*orderv1.OrderItem) ([]order.Item, error) {
	out := make([]order.Item, 0, len(in))
	for idx, pi := range in {
		item, err := fromProtoItem(pi)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", idx, err)
		}
		out = append(out, item)
	}
	return out, nil
}

func toProtoOrder(o order.Order) *orderv1.Order {
	items := make([]*orderv1.OrderItem, 0, len(o.Items))
	for _, i := range o.Items {
		items = append(items, toProtoItem(i))
	}
	return &orderv1.Order{
		OrderId:    o.ID,
		TenantId:   o.TenantID,
		CustomerId: o.CustomerID,
		Status:     toProtoStatus(o.Status),
		Items:      items,
		Total:      toProtoMoney(o.Total),
		CreatedAt:  timestamppb.New(o.CreatedAt),
		UpdatedAt:  timestamppb.New(o.UpdatedAt),
	}
}

func toProtoOrders(in []order.Order) []*orderv1.Order {
	out := make([]*orderv1.Order, 0, len(in))
	for _, o := range in {
		out = append(out, toProtoOrder(o))
	}
	return out
}

// fromProtoOrder exists for round-trip testing. Production request paths build domain
// objects from COMMANDS (CreateOrderRequest), not from a full Order message -- accepting
// a client-supplied Order wholesale would let a caller set its own id, tenant, status and
// total.
func fromProtoOrder(p *orderv1.Order) (order.Order, error) {
	if p == nil {
		return order.Order{}, fmt.Errorf("order is nil")
	}
	items, err := fromProtoItems(p.GetItems())
	if err != nil {
		return order.Order{}, err
	}
	total, err := fromProtoMoney(p.GetTotal())
	if err != nil {
		return order.Order{}, err
	}
	return order.Order{
		ID:         p.GetOrderId(),
		TenantID:   p.GetTenantId(),
		CustomerID: p.GetCustomerId(),
		Status:     fromProtoStatus(p.GetStatus()),
		Items:      items,
		Total:      total,
		CreatedAt:  p.GetCreatedAt().AsTime(),
		UpdatedAt:  p.GetUpdatedAt().AsTime(),
	}, nil
}
