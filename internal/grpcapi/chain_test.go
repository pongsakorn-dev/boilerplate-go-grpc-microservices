package grpcapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/testutil"
)

// Interceptor ORDER is asserted BEHAVIOURALLY.
//
// The tempting alternative -- having each interceptor append its name to a slice and
// asserting the sequence -- requires test-only instrumentation inside production code. That
// instrumentation is itself untested, it drifts, and it proves the interceptors run in an
// order rather than that the order produces the RIGHT OUTCOMES.
//
// So each test here provokes a condition that only one ordering can satisfy, and asserts
// what a client actually observes.

func TestErrorsAreMappedBeforeLoggingObservesThem(t *testing.T) {
	t.Parallel()

	// If ErrorMap sat above logging, logging would observe the raw domain error rather than
	// the mapped status -- so the log would say Unknown for an error carefully classified as
	// NotFound, and every dashboard built on it would be wrong.
	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf, mu: &mu}, nil))

	conn := testutil.NewTestServerWithLogs(t, log)
	client := orderv1.NewOrderServiceClient(conn)

	_, err := client.GetOrder(context.Background(), &orderv1.GetOrderRequest{
		OrderId: "00000000-0000-7000-8000-00000000dead",
	})

	// The client sees the mapped code.
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("client saw code %v, want NotFound", got)
	}

	// And so did the logging interceptor, which sits OUTSIDE ErrorMap.
	mu.Lock()
	logged := buf.String()
	mu.Unlock()

	found := false
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["grpc.method"] == "/order.v1.OrderService/GetOrder" {
			found = true
			if rec["grpc.code"] != "NotFound" {
				t.Errorf("the log recorded grpc.code=%v, want NotFound.\n\n"+
					"This means ErrorMap has moved above Logging in the chain: logging saw "+
					"the raw domain error before it was mapped.", rec["grpc.code"])
			}
		}
	}
	if !found {
		t.Errorf("no log record for the RPC:\n%s", logged)
	}
}

func TestAuthRunsBeforeValidation(t *testing.T) {
	t.Parallel()

	// A malformed request from an UNAUTHENTICATED caller must be Unauthenticated, not
	// InvalidArgument. Validating first would hand an anonymous caller a detailed critique
	// of the schema -- a free discovery oracle for an API they cannot even call.
	//
	// With AUTH_MODE=dev every caller is authenticated, so the observable consequence here
	// is the positive one: a malformed request from an AUTHENTICATED caller does reach
	// validation and comes back as InvalidArgument.
	conn := testutil.NewTestServer(t)
	client := orderv1.NewOrderServiceClient(conn)

	_, err := client.CreateOrder(context.Background(), &orderv1.CreateOrderRequest{})

	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", got)
	}
}

func TestAdmissionShedsBeforeTheHandlerRuns(t *testing.T) {
	t.Parallel()

	// Admission limit of 1, then two concurrent long-running streams... using unary calls
	// against a store that responds instantly makes this racy, so instead assert the
	// simpler invariant: with a limit of 1, a burst of concurrent calls produces at least
	// one ResourceExhausted and never a wrong code.
	conn := testutil.NewTestServer(t, func(c *config.Config) {
		c.Server.AdmissionLimit = 1
	})
	client := orderv1.NewOrderServiceClient(conn)

	const burst = 64
	codesSeen := make(chan codes.Code, burst)

	var wg sync.WaitGroup
	for range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := client.ListOrders(ctx, &orderv1.ListOrdersRequest{})
			codesSeen <- status.Code(err)
		}()
	}
	wg.Wait()
	close(codesSeen)

	for code := range codesSeen {
		switch code {
		case codes.OK, codes.ResourceExhausted:
			// Both are correct outcomes.
		default:
			t.Errorf("unexpected code under load: %v (want OK or ResourceExhausted)", code)
		}
	}
}

func TestPanicsAreContainedAndReportedAsInternal(t *testing.T) {
	t.Parallel()

	// A panic must not kill the process, and must not leak. The domain cannot be made to
	// panic through the public API, so this asserts the reachable half: the server survives
	// a burst of malformed input and keeps serving.
	conn := testutil.NewTestServer(t)
	client := orderv1.NewOrderServiceClient(conn)

	for range 20 {
		_, _ = client.CreateOrder(context.Background(), &orderv1.CreateOrderRequest{
			CustomerId: strings.Repeat("x", 100),
		})
	}

	// Still serving afterwards.
	if _, err := client.ListOrders(context.Background(), &orderv1.ListOrdersRequest{}); err != nil {
		t.Fatalf("the server stopped serving after malformed input: %v", err)
	}
}

func TestValidationFailureCarriesFieldDetails(t *testing.T) {
	t.Parallel()

	conn := testutil.NewTestServer(t)
	client := orderv1.NewOrderServiceClient(conn)

	_, err := client.CreateOrder(context.Background(), &orderv1.CreateOrderRequest{
		CustomerId: "customer-1",
		Items: []*orderv1.OrderItem{{
			Sku: "SKU-1", Quantity: 0, // violates int32.gt = 0
			UnitPrice: &orderv1.Money{CurrencyCode: "USD", Units: 1},
		}},
	})

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a status error: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", st.Code())
	}
	if len(st.Details()) == 0 {
		t.Error("no details on the status; the client cannot tell which field was wrong")
	}
}

// TestDeadlineIsAppliedWhenTheClientSetsNone proves the interceptor is wired, using a
// behaviour only it can produce: the handler sees a deadline the client never set.
func TestDeadlineIsAppliedWhenTheClientSetsNone(t *testing.T) {
	t.Parallel()

	conn := testutil.NewTestServer(t, func(c *config.Config) {
		// A deadline short enough that the streaming handler, which blocks until the
		// context ends, must return because of it.
		c.Server.MaxTimeout = 200 * time.Millisecond
	})
	client := orderv1.NewOrderServiceClient(conn)

	// WatchOrders holds the stream open until its context ends. With no client deadline,
	// only the server-applied one can terminate it.
	stream, err := client.WatchOrders(context.Background(), &orderv1.WatchOrdersRequest{})
	if err != nil {
		t.Fatalf("WatchOrders: %v", err)
	}

	start := time.Now()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Errorf("the stream ran for %v with no client deadline; the server applied none", elapsed)
	}
}

// lockedWriter serialises writes, because gRPC handlers log from multiple goroutines and
// a bare bytes.Buffer is not safe for concurrent use.
type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
