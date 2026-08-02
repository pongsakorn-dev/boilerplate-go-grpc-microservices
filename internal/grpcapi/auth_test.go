package grpcapi_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
	"github.com/example/gomicro/internal/platform/auth/testjwks"
	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/testutil"
)

// End-to-end authentication, through the REAL chain.
//
// These drive the production server built by app.New -- the real interceptors, the real
// verifier selected by AUTH_MODE, the real policy -- over bufconn, against an in-process
// issuer. Nothing is stubbed, and the whole file runs with no Docker, no network and no
// identity provider.
//
// That combination is the point. Unit tests on the verifier prove tokens are checked; only
// these prove the checking is WIRED IN. The bypass this milestone exists to close was
// exactly that gap: a correct verifier would not have helped, because the server never
// called one.

// oidcServer starts the production server in OIDC mode against an in-process issuer.
func oidcServer(t *testing.T, iss *testjwks.Issuer) *grpc.ClientConn {
	t.Helper()

	return testutil.NewTestServer(t, func(c *config.Config) {
		c.AuthMode = config.AuthOIDC
		c.OIDC.IssuerURL = iss.URL()
		c.OIDC.Audience = iss.Audience
		c.OIDC.TenantClaim = iss.TenantClaim
		c.OIDC.ScopeClaim = iss.ScopeClaim
	})
}

// withToken attaches a bearer credential the way a real client does.
func withToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

func validItems() []*orderv1.OrderItem {
	return []*orderv1.OrderItem{{
		Sku:       "SKU-1",
		Quantity:  1,
		UnitPrice: &orderv1.Money{CurrencyCode: "USD", Units: 10},
	}}
}

// TestUnauthenticatedCallsAreRefused is the regression test for the bypass.
//
// The literal reproduction of that bug: call an RPC with no credentials whatsoever and see
// what comes back. It used to be three orders, served as a full-scope dev-tenant principal,
// with AUTH_MODE=oidc set and APP_ENV=production.
func TestUnauthenticatedCallsAreRefused(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	client := orderv1.NewOrderServiceClient(oidcServer(t, iss))

	resp, err := client.ListOrders(context.Background(), &orderv1.ListOrdersRequest{})
	if err == nil {
		t.Fatalf("an unauthenticated ListOrders returned %d orders. This is the exact bypass "+
			"milestone M5 exists to close.", len(resp.GetOrders()))
	}
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", got)
	}
}

func TestAValidTokenIsAccepted(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	client := orderv1.NewOrderServiceClient(oidcServer(t, iss))

	ctx := withToken(context.Background(), iss.Sign(iss.DefaultClaims()))
	resp, err := client.CreateOrder(ctx, &orderv1.CreateOrderRequest{
		CustomerId: "cust-1",
		Items:      validItems(),
	})
	if err != nil {
		t.Fatalf("a valid token with orders:write was refused: %v", err)
	}

	// The tenant on the created order came from the TOKEN, not from anything the client
	// sent -- the request message has no tenant field at all.
	if got := resp.GetOrder().GetTenantId(); got != "tenant-a" {
		t.Errorf("order tenant = %q, want tenant-a (the tenant in the verified token)", got)
	}
}

// TestScopesAreEnforcedEndToEnd proves the read/write split is a control rather than
// documentation.
func TestScopesAreEnforcedEndToEnd(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	client := orderv1.NewOrderServiceClient(oidcServer(t, iss))

	claims := iss.DefaultClaims()
	claims[iss.ScopeClaim] = "orders:read" // no write
	readOnly := withToken(context.Background(), iss.Sign(claims))

	_, err := client.CreateOrder(readOnly, &orderv1.CreateOrderRequest{
		CustomerId: "cust-1",
		Items:      validItems(),
	})
	if err == nil {
		t.Fatal("a read-only credential created an order")
	}
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", got)
	}

	// The SAME credential must still be able to read.
	if _, err := client.ListOrders(readOnly, &orderv1.ListOrdersRequest{}); err != nil {
		t.Errorf("a read-only credential was denied a read: %v", err)
	}
}

// TestDenialsDoNotLeakWhichScopeIsMissing keeps the error message from becoming a shopping
// list.
//
// "requires scope orders:write" tells an attacker precisely what to go phishing for, or
// which role to ask an over-helpful administrator to add. The reason belongs in the log,
// where the operator is; the client gets a code.
func TestDenialsDoNotLeakWhichScopeIsMissing(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	client := orderv1.NewOrderServiceClient(oidcServer(t, iss))

	claims := iss.DefaultClaims()
	claims[iss.ScopeClaim] = "orders:read"

	_, err := client.CreateOrder(withToken(context.Background(), iss.Sign(claims)),
		&orderv1.CreateOrderRequest{CustomerId: "cust-1", Items: validItems()})
	if err == nil {
		t.Fatal("expected a denial")
	}

	msg := status.Convert(err).Message()
	for _, leak := range []string{"orders:write", "scope", "requires"} {
		if strings.Contains(strings.ToLower(msg), leak) {
			t.Errorf("the client-visible message %q names %q; that is a shopping list for an attacker", msg, leak)
		}
	}
}

// TestTenantIsolationIsDrivenByTheToken is the assertion internal/grpcapi/server.go's
// tenantOf comment has been promising since M1.
//
// The domain contract suite already proves the STORE isolates tenants. This proves the
// tenant reaching the store is the one from the verified token -- the half no store test can
// reach, and the half that fails if anyone ever adds a tenant_id to a request message.
func TestTenantIsolationIsDrivenByTheToken(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	client := orderv1.NewOrderServiceClient(oidcServer(t, iss))

	tokenFor := func(tenant string) context.Context {
		claims := iss.DefaultClaims()
		claims[iss.TenantClaim] = tenant
		claims["sub"] = "user-of-" + tenant
		return withToken(context.Background(), iss.Sign(claims))
	}

	created, err := client.CreateOrder(tokenFor("tenant-a"), &orderv1.CreateOrderRequest{
		CustomerId: "cust-a",
		Items:      validItems(),
	})
	if err != nil {
		t.Fatalf("tenant-a create: %v", err)
	}
	orderID := created.GetOrder().GetOrderId()

	// tenant-b holds a perfectly valid credential with every scope. The only thing it does
	// not have is any relationship to this order.
	_, err = client.GetOrder(tokenFor("tenant-b"), &orderv1.GetOrderRequest{OrderId: orderID})
	if err == nil {
		t.Fatal("tenant-b read tenant-a's order. Multi-tenancy is not enforced.")
	}

	// NotFound, not PermissionDenied. PermissionDenied confirms the order exists, which
	// turns GetOrder into an oracle for enumerating other tenants' order ids.
	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("code = %v, want NotFound -- another tenant's order must be indistinguishable "+
			"from one that does not exist, or the error itself confirms it exists", got)
	}

	// And tenant-a can still read it, so the test is not passing because reads are broken.
	if _, err := client.GetOrder(tokenFor("tenant-a"), &orderv1.GetOrderRequest{OrderId: orderID}); err != nil {
		t.Fatalf("tenant-a could not read its own order: %v", err)
	}
}

// TestHealthIsReachableWithoutCredentials protects a self-inflicted outage.
//
// Kubernetes' native grpc: probe dials the port and calls Check with no metadata at all. If
// this ever requires authentication, every pod fails readiness, never joins the Service, and
// the deployment looks like a crashloop with no clue pointing at the policy.
func TestHealthIsReachableWithoutCredentials(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	conn := oidcServer(t, iss)

	resp, err := healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("an anonymous health check was refused: %v\n\n"+
			"A kubelet holds no credential. This makes every pod fail its readiness probe.", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("status = %v, want SERVING", resp.GetStatus())
	}
}

// TestReflectionRequiresAuthentication is the other side of the health exemption.
//
// Reflection hands the caller the entire schema. Public health plus public reflection would
// mean an anonymous stranger can enumerate every service, message and field you have.
func TestReflectionRequiresAuthentication(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	conn := oidcServer(t, iss)

	stream, err := grpc.NewClientStream(context.Background(),
		&grpc.StreamDesc{StreamName: "ServerReflectionInfo", ServerStreams: true, ClientStreams: true},
		conn, "/grpc.reflection.v1.ServerReflection/ServerReflectionInfo")
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	// The interceptor rejects before the handler runs, so the error surfaces on the first
	// receive rather than at stream creation.
	_ = stream.CloseSend()
	err = stream.RecvMsg(new(struct{}))

	if got := status.Code(err); got != codes.Unauthenticated {
		t.Errorf("anonymous reflection got code %v, want Unauthenticated -- reflection "+
			"publishes your whole schema", got)
	}
}

// TestExpiredTokensAreRefusedThroughTheChain confirms expiry survives the wiring, not just
// the verifier's own unit test.
func TestExpiredTokensAreRefusedThroughTheChain(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	client := orderv1.NewOrderServiceClient(oidcServer(t, iss))

	claims := iss.DefaultClaims()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()

	_, err := client.ListOrders(withToken(context.Background(), iss.Sign(claims)),
		&orderv1.ListOrdersRequest{})
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated for an expired token", got)
	}
}

// TestStreamingHandlersSeeThePrincipal covers the failure invisible to every unary test.
//
// grpc.ServerStream returns its own context and has no setter, so an interceptor that adds
// the principal must replace the stream object entirely. Forget the wrapper and every
// streaming handler calling auth.TenantFrom finds nothing -- while all the unary tests stay
// green, because unary interceptors pass a context directly.
func TestStreamingHandlersSeeThePrincipal(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	client := orderv1.NewOrderServiceClient(oidcServer(t, iss))

	ctx, cancel := context.WithTimeout(withToken(context.Background(), iss.Sign(iss.DefaultClaims())), 10*time.Second)
	defer cancel()

	// Create an order first. WatchOrders replays the tenant's existing orders before it
	// blocks, so the stream produces a message immediately -- which makes this an assertion
	// about the tenant that reached the handler rather than about a timeout elapsing without
	// an error. The timeout version passed in 5 seconds by proving almost nothing.
	if _, err := client.CreateOrder(ctx, &orderv1.CreateOrderRequest{
		CustomerId: "cust-stream",
		Items:      validItems(),
	}); err != nil {
		t.Fatalf("seed order: %v", err)
	}

	stream, err := client.WatchOrders(ctx, &orderv1.WatchOrdersRequest{})
	if err != nil {
		t.Fatalf("open watch stream: %v", err)
	}

	msg, err := stream.Recv()
	if err != nil {
		if status.Code(err) == codes.Unauthenticated {
			t.Fatalf("a streaming handler could not see the principal: %v\n\n"+
				"AuthStream must wrap the ServerStream to override Context(): grpc.ServerStream "+
				"returns its own context and has no setter, so without the wrapper every "+
				"streaming handler calling auth.TenantFrom finds nothing -- while every unary "+
				"test stays green.", err)
		}
		t.Fatalf("recv: %v", err)
	}

	// The tenant on the streamed order came from the token, through the wrapped stream
	// context, into the handler.
	if got := msg.GetOrder().GetTenantId(); got != "tenant-a" {
		t.Errorf("streamed order tenant = %q, want tenant-a", got)
	}
}
