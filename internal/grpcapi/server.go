package grpcapi

import (
	"context"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
	"github.com/example/gomicro/internal/order"
	"github.com/example/gomicro/internal/platform/apperr"
	"github.com/example/gomicro/internal/platform/auth"
)

// OrderServer adapts the domain service to the generated gRPC interface.
//
// Every method here does the same three things and nothing else: take the tenant from
// the VERIFIED principal, translate proto to domain, and translate domain errors back.
// Business rules live in internal/order. When a handler starts growing logic, that logic
// belongs one layer down where it can be tested without a transport.
type OrderServer struct {
	orderv1.UnimplementedOrderServiceServer

	svc *order.Service
}

// NewOrderServer builds the gRPC adapter.
func NewOrderServer(svc *order.Service) *OrderServer {
	return &OrderServer{svc: svc}
}

// tenantOf takes the tenant from the authenticated principal.
//
// It deliberately ignores any tenant_id present in the request message. A caller that
// could name its own tenant would be able to read and write every other tenant's data
// with a valid token of its own -- the single worst bug a multi-tenant service can have.
// auth/tenant_isolation_test.go asserts a body-supplied tenant is ignored.
func tenantOf(ctx context.Context) (string, error) {
	tenant, ok := auth.TenantFrom(ctx)
	if !ok {
		return "", apperr.New(apperr.KindUnauthenticated, "NO_PRINCIPAL",
			"request reached a handler without a verified principal")
	}
	return tenant, nil
}

func (s *OrderServer) CreateOrder(ctx context.Context, req *orderv1.CreateOrderRequest) (*orderv1.CreateOrderResponse, error) {
	tenant, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}

	items, err := fromProtoItems(req.GetItems())
	if err != nil {
		return nil, asAppError(err)
	}

	created, err := s.svc.Create(ctx, order.CreateCommand{
		TenantID:   tenant,
		CustomerID: req.GetCustomerId(),
		Items:      items,
	})
	if err != nil {
		return nil, asAppError(err)
	}
	return &orderv1.CreateOrderResponse{Order: toProtoOrder(created)}, nil
}

func (s *OrderServer) GetOrder(ctx context.Context, req *orderv1.GetOrderRequest) (*orderv1.GetOrderResponse, error) {
	tenant, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}

	got, err := s.svc.Get(ctx, tenant, req.GetOrderId())
	if err != nil {
		return nil, asAppError(err)
	}
	return &orderv1.GetOrderResponse{Order: toProtoOrder(got)}, nil
}

func (s *OrderServer) ListOrders(ctx context.Context, req *orderv1.ListOrdersRequest) (*orderv1.ListOrdersResponse, error) {
	tenant, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}

	page, err := s.svc.List(ctx, tenant, order.ListFilter{
		Status:     fromProtoStatus(req.GetStatus()),
		CustomerID: req.GetCustomerId(),
		PageSize:   req.GetPageSize(),
		PageToken:  req.GetPageToken(),
	})
	if err != nil {
		return nil, asAppError(err)
	}
	return &orderv1.ListOrdersResponse{
		Orders:        toProtoOrders(page.Orders),
		NextPageToken: page.NextPageToken,
	}, nil
}

func (s *OrderServer) CancelOrder(ctx context.Context, req *orderv1.CancelOrderRequest) (*orderv1.CancelOrderResponse, error) {
	tenant, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}

	cancelled, err := s.svc.Cancel(ctx, tenant, req.GetOrderId())
	if err != nil {
		return nil, asAppError(err)
	}
	return &orderv1.CancelOrderResponse{Order: toProtoOrder(cancelled)}, nil
}

// WatchOrders streams matching orders and then holds the stream open.
//
// The template ships one streaming RPC on purpose. A stream is the only way to exercise
// three things that are completely invisible to unary tests: the grpc.ServerStream
// context wrapper (so interceptor-injected values reach a stream handler), mid-stream
// client cancellation, and the graceful-drain path where an in-flight stream must finish
// before Serve returns.
//
// A production watch would subscribe to the event stream rather than sending a snapshot
// and waiting; that seam is where you would plug in the JetStream consumer.
func (s *OrderServer) WatchOrders(req *orderv1.WatchOrdersRequest, stream orderv1.OrderService_WatchOrdersServer) error {
	ctx := stream.Context()

	tenant, err := tenantOf(ctx)
	if err != nil {
		return err
	}

	page, err := s.svc.List(ctx, tenant, order.ListFilter{Status: fromProtoStatus(req.GetStatus())})
	if err != nil {
		return asAppError(err)
	}

	for _, o := range page.Orders {
		if err := stream.Send(&orderv1.WatchOrdersResponse{Order: toProtoOrder(o)}); err != nil {
			return err
		}
		// Send can succeed after the client has gone away, so the cancellation check has
		// to be explicit rather than relying on Send to report it.
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	// Hold the stream open until the client cancels or the server drains.
	<-ctx.Done()
	return ctx.Err()
}

// Compile-time proof the adapter implements the generated interface. Without this, adding
// an RPC to the proto only fails wherever the server happens to be registered.
var _ orderv1.OrderServiceServer = (*OrderServer)(nil)
