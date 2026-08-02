package client_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
	"github.com/example/gomicro/internal/platform/apperr"
	"github.com/example/gomicro/internal/platform/client"
	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/testutil"
)

// dialRealServer points this package's client at the PRODUCTION server.
//
// Everything else in this package tests against a controllable fake, which is right for
// asserting client behaviour and wrong for asserting that the two halves fit. The real server
// has nine interceptors, a default-deny policy, hardened transport options and its own
// deadline clamp; a client that satisfies a hand-written stub and not that is a client that
// works nowhere.
func dialRealServer(t *testing.T, mutate ...func(*client.Options)) (*grpc.ClientConn, config.Config) {
	t.Helper()

	cfg := testConfig(t)
	dialOpts := testutil.NewTestServerDialer(t)

	opts := client.New(cfg, "passthrough:///orderd")
	opts.TransportCredentials = client.Insecure()
	opts.DialOptions = dialOpts
	for _, fn := range mutate {
		fn(&opts)
	}

	conn, err := client.Dial(cfg, opts)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, cfg
}

// TestARealServerAcceptsWhatThisClientSends is the hop, end to end.
func TestARealServerAcceptsWhatThisClientSends(t *testing.T) {
	t.Parallel()

	conn, _ := dialRealServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := orderv1.NewOrderServiceClient(conn).ListOrders(ctx, &orderv1.ListOrdersRequest{})
	if err != nil {
		t.Fatalf("ListOrders through the real chain: %v", err)
	}
	if len(resp.GetOrders()) == 0 {
		t.Error("the seeded orders did not come back, so the call did not really reach the store")
	}
}

// TestAnUpstreamFailureIsNotReturnableToYourOwnCallers is the error-model assertion, and the
// reason *client.Error exists at all.
//
// The tempting implementation returns the upstream's error unchanged. It compiles, it looks
// right, and what happens next is that this service's ErrorMap interceptor sees a valid
// *status.Error that is not an *apperr.Error and forwards it verbatim -- so YOUR caller
// receives the upstream's code, the upstream's message, and an ErrorInfo naming the upstream's
// service. They conclude that the resource THEY asked for does not exist.
//
// A *client.Error is not a *status.Error, so that mistake cannot be made by accident.
func TestAnUpstreamFailureIsNotReturnableToYourOwnCallers(t *testing.T) {
	t.Parallel()

	conn, _ := dialRealServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := orderv1.NewOrderServiceClient(conn).GetOrder(ctx, &orderv1.GetOrderRequest{
		OrderId: "019fc26c-0000-7000-8000-000000000000",
	})
	if err == nil {
		t.Fatal("fetching a nonexistent order succeeded")
	}

	var ce *client.Error
	if !errors.As(err, &ce) {
		t.Fatalf("the upstream failure is a %T, not a *client.Error.\n\n"+
			"Returned from a handler it would be forwarded verbatim and your caller would "+
			"read the upstream's NotFound as being about their own request.", err)
	}

	// It carries what an operator needs.
	if ce.Code != codes.NotFound {
		t.Errorf("Code = %s, want NotFound", ce.Code)
	}
	if ce.Reason != "ORDER_NOT_FOUND" {
		t.Errorf("Reason = %q, want the callee's ErrorInfo reason to survive", ce.Reason)
	}
	if ce.Domain == "" {
		t.Error("Domain is empty; the callee's service name is how you tell whose NotFound this is")
	}

	// And the default translation is deliberately NOT the callee's code.
	//
	// The callee said NotFound about ITS resource. Your caller did not ask for that resource
	// -- you did -- so from where they sit this is your service failing, and Internal is the
	// honest name for it. Saying NotFound instead sends them hunting for an id that is fine.
	if got := ce.Kind(); got != apperr.KindInternal {
		t.Errorf("Kind() = %v, want KindInternal.\n\n"+
			"Passing the callee's NotFound through tells your caller their own request was "+
			"for something that does not exist.", got)
	}
}

// TestATranslatedUpstreamErrorKeepsTheCauseCovers the deliberate path: when a callee's answer
// really does mean something in your domain, you say so explicitly.
func TestATranslatedUpstreamErrorKeepsTheCause(t *testing.T) {
	t.Parallel()

	conn, _ := dialRealServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := orderv1.NewOrderServiceClient(conn).GetOrder(ctx, &orderv1.GetOrderRequest{
		OrderId: "019fc26c-0000-7000-8000-000000000000",
	})
	ce, ok := client.From(err)
	if !ok {
		t.Fatalf("not a client error: %v", err)
	}

	// The shape a handler writes when the upstream's answer is genuinely about the caller's
	// input -- "the SKU you asked for does not exist" really is an InvalidArgument on the
	// order they submitted.
	translated := ce.AppError(apperr.KindInvalidArgument, "ORDER_ITEM_INVALID", "no such item")

	if got := apperr.KindOf(translated); got != apperr.KindInvalidArgument {
		t.Errorf("Kind = %v, want the translation to win", got)
	}

	// The upstream error survives as the cause, so it is still in the logs and still
	// reachable by errors.As -- while the caller sees only what was chosen for them.
	var still *client.Error
	if !errors.As(translated, &still) {
		t.Error("translating discarded the upstream cause; the original failure is now " +
			"invisible to anything downstream of the log line")
	}

	// And converting it for the wire redacts nothing it should not: the reason is the one
	// that was chosen, and the domain is THIS service.
	st, _ := status.FromError(apperr.ToError(translated, "orderd"))
	if st.Code() != codes.InvalidArgument {
		t.Errorf("wire code = %s, want InvalidArgument", st.Code())
	}
}

// TestTheBudgetSurvivesTheRealServersDeadlineClamp checks the two mechanisms compose.
//
// The server applies its own clamp (DEFAULT_TIMEOUT when the caller sets none, MAX_TIMEOUT as
// a ceiling). The client applies a budget. Both are subtractions on the same propagated
// deadline, and getting the direction wrong on either would show up as a call that gets MORE
// time than its caller allowed.
func TestTheBudgetSurvivesTheRealServersDeadlineClamp(t *testing.T) {
	t.Parallel()

	conn, _ := dialRealServer(t)

	const budget = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	if _, err := orderv1.NewOrderServiceClient(conn).ListOrders(ctx, &orderv1.ListOrdersRequest{}); err != nil {
		t.Fatalf("ListOrders: %v", err)
	}
	if elapsed := time.Since(start); elapsed > budget {
		t.Errorf("the call took %s against a %s budget", elapsed, budget)
	}
}
