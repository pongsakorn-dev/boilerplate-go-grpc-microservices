package interceptor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// THE CONTRACT ASSERTED HERE IS THE STATUS A CLIENT RECEIVES, not an intermediate value.
//
// The previous version of this file checked apperr.ToStatus(err, "orders") -- performing the
// mapping step by hand. Production did not perform it: Recovery is the OUTERMOST interceptor,
// so nothing above it maps what it returns, and grpc-go fell back to
// codes.Unknown + err.Error(), which embeds the panic value. The tests were green while a
// panic on a connection string sent the DSN and its password to the caller.
//
// A test that supplies a step production omits will pass forever. So these assert on the
// status as grpc-go would derive it, and TestRecoveryThroughTheRealChain drives the real
// interceptors in their real order.

// captureLogs returns a logger writing JSON into buf, for asserting on records.
func captureLogs() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// TestRecoveryContainsPanics asserts the two things that matter, for every panic shape.
//
// A panic in one RPC must not take down the process: every other in-flight request on that
// pod would die with it, turning one bad request into an outage. And the panic value must
// not reach the client: it routinely contains the argument that caused it, and the stack
// names your packages and file paths.
func TestRecoveryContainsPanics(t *testing.T) {
	t.Parallel()

	panics := map[string]any{
		"string":    "boom at /var/secrets/db-password",
		"error":     errors.New("nil map write in orderStore.Save"),
		"non-error": struct{ Detail string }{"internal state"},
		"nil deref": nil, // panic(nil) becomes a *runtime.PanicNilError in Go 1.21+
		"int":       42,
	}

	for name, value := range panics {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			log, buf := captureLogs()
			intercept := Recovery(log, "orders")

			handler := func(ctx context.Context, req any) (any, error) { panic(value) }

			// The process survives: reaching the next line at all is the assertion.
			resp, err := intercept(context.Background(), nil, testInfo, handler)

			if resp != nil {
				t.Errorf("resp = %v, want nil", resp)
			}
			if err == nil {
				t.Fatal("a panic produced no error")
			}

			// status.FromError, exactly as grpc-go derives what goes on the wire.
			st, ok := status.FromError(err)
			if !ok {
				t.Fatalf("Recovery returned an error carrying no gRPC status (%T); grpc-go will "+
					"send codes.Unknown with the raw error text, which embeds the panic value", err)
			}
			if st.Code() != codes.Internal {
				t.Errorf("code = %v, want Internal", st.Code())
			}
			if st.Message() != "internal error" {
				t.Errorf("client message = %q, want %q", st.Message(), "internal error")
			}

			// But the log has everything needed to debug it.
			logged := buf.String()
			if !strings.Contains(logged, "panic") {
				t.Errorf("the log has no panic field:\n%s", logged)
			}
			if !strings.Contains(logged, "stack") {
				t.Errorf("the log has no stack trace, so the panic is undebuggable:\n%s", logged)
			}
			if !strings.Contains(logged, testInfo.FullMethod) {
				t.Errorf("the log does not name the method:\n%s", logged)
			}
		})
	}
}

// TestRecoveryDoesNotLeakPanicTextIntoTheStatus is stated separately because it is the
// security-relevant half.
func TestRecoveryDoesNotLeakPanicTextIntoTheStatus(t *testing.T) {
	t.Parallel()

	const secret = "postgres://user:hunter2@db.internal:5432"

	log, buf := captureLogs()
	intercept := Recovery(log, "orders")

	handler := func(ctx context.Context, req any) (any, error) { panic(secret) }
	_, err := intercept(context.Background(), nil, testInfo, handler)

	// status.FromError, NOT apperr.ToStatus. Calling ToStatus here is what hid this bug: it
	// applied the redaction that production never reached.
	st, _ := status.FromError(err)
	if strings.Contains(st.Message(), "hunter2") || strings.Contains(st.Message(), "db.internal") {
		t.Errorf("the status leaks the panic value: %q", st.Message())
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error grpc-go serialises still contains the secret: %q", err.Error())
	}

	// Never leak, never lose: it must still be in the log.
	if !strings.Contains(buf.String(), "hunter2") {
		t.Error("the panic value was lost from the log; the panic is now undebuggable")
	}
}

// TestRecoveryStreamGuardsStreamingHandlers exists because a unary interceptor NEVER runs
// for a streaming RPC.
//
// Ship only the unary guard and every streaming handler is unprotected -- and streaming
// handlers are the long-lived ones most likely to hit an edge case hours into a connection.
func TestRecoveryStreamGuardsStreamingHandlers(t *testing.T) {
	t.Parallel()

	log, buf := captureLogs()
	intercept := RecoveryStream(log, "orders")

	handler := func(srv any, ss grpc.ServerStream) error { panic("stream boom") }

	err := intercept(nil, fakeStream{ctx: context.Background()},
		&grpc.StreamServerInfo{FullMethod: "/order.v1.OrderService/WatchOrders"}, handler)

	if err == nil {
		t.Fatal("a panic in a stream handler produced no error")
	}
	if st, _ := status.FromError(err); st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
	if !strings.Contains(buf.String(), "stream boom") {
		t.Error("the stream panic was not logged")
	}
}

// TestRecoveryLogsExactlyOneRecord guards against the "two recovery interceptors" pattern,
// which produces duplicate alerts for a single incident.
func TestRecoveryLogsExactlyOneRecord(t *testing.T) {
	t.Parallel()

	log, buf := captureLogs()
	intercept := Recovery(log, "orders")

	handler := func(ctx context.Context, req any) (any, error) { panic("once") }
	_, _ = intercept(context.Background(), nil, testInfo, handler)

	count := 0
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		count++
	}
	if count != 1 {
		t.Errorf("logged %d records for one panic, want exactly 1", count)
	}
}

// fakeStream is a minimal grpc.ServerStream for interceptor tests.
type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f fakeStream) Context() context.Context { return f.ctx }

// TestRecoveryThroughTheRealChain is the test that would have caught the leak, and the reason
// the others above were not enough.
//
// Every previous recovery test called Recovery in isolation. In isolation it looks correct.
// The bug lived in the RELATIONSHIP between Recovery and ErrorMap: Recovery is outermost, so
// ErrorMap -- which sits below it -- never sees what Recovery returns. Only composing them in
// production order shows it.
//
// chain.go's comment states the rule this violated: ErrorMap must sit above every interceptor
// that produces an error. Recovery produces one and sits above ErrorMap, and that exception
// went unnoticed for two milestones because nothing composed them.
func TestRecoveryThroughTheRealChain(t *testing.T) {
	t.Parallel()

	const secret = "postgres://user:hunter2@db.internal:5432/orders"

	log, _ := captureLogs()

	// The production relative order from internal/grpcapi/chain.go.
	chain := []grpc.UnaryServerInterceptor{
		Recovery(log, "orders"),
		Logging(log),
		ErrorMap("orders"),
	}

	handler := func(context.Context, any) (any, error) { panic(secret) }

	// Compose outermost-first, the way grpc.ChainUnaryInterceptor does.
	var invoke func(i int, ctx context.Context, req any) (any, error)
	invoke = func(i int, ctx context.Context, req any) (any, error) {
		if i == len(chain) {
			return handler(ctx, req)
		}
		return chain[i](ctx, req, testInfo, func(c context.Context, r any) (any, error) {
			return invoke(i+1, c, r)
		})
	}

	_, err := invoke(0, context.Background(), nil)

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("no gRPC status on the chain's error (%T)", err)
	}
	if st.Code() == codes.Unknown {
		t.Errorf("code = Unknown. Nothing mapped the recovered panic, so grpc-go fell back to " +
			"status.New(codes.Unknown, err.Error()) -- and err.Error() embeds the panic value. " +
			"Meshes and dashboards also misclassify Unknown.")
	}
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
	if st.Message() != "internal error" {
		t.Errorf("client message = %q, want \"internal error\".\n\n"+
			"A panic value routinely contains the argument that caused it -- here a database "+
			"connection string, password included.", st.Message())
	}
}
