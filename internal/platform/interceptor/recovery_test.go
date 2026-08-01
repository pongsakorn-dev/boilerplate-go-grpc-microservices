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

	"github.com/example/gomicro/internal/platform/apperr"
)

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
			intercept := Recovery(log)

			handler := func(ctx context.Context, req any) (any, error) { panic(value) }

			// The process survives: reaching the next line at all is the assertion.
			resp, err := intercept(context.Background(), nil, testInfo, handler)

			if resp != nil {
				t.Errorf("resp = %v, want nil", resp)
			}
			if err == nil {
				t.Fatal("a panic produced no error")
			}
			if kind := apperr.KindOf(err); kind != apperr.KindInternal {
				t.Errorf("kind = %v, want Internal", kind)
			}

			// The client sees nothing about the panic.
			appErr, _ := apperr.From(err)
			if msg := appErr.ClientMessage(); msg != "internal error" {
				t.Errorf("ClientMessage() = %q, want %q", msg, "internal error")
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
	intercept := Recovery(log)

	handler := func(ctx context.Context, req any) (any, error) { panic(secret) }
	_, err := intercept(context.Background(), nil, testInfo, handler)

	st := apperr.ToStatus(err, "orders")
	if strings.Contains(st.Message(), "hunter2") || strings.Contains(st.Message(), "db.internal") {
		t.Errorf("the status leaks the panic value: %q", st.Message())
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
	intercept := RecoveryStream(log)

	handler := func(srv any, ss grpc.ServerStream) error { panic("stream boom") }

	err := intercept(nil, fakeStream{ctx: context.Background()},
		&grpc.StreamServerInfo{FullMethod: "/order.v1.OrderService/WatchOrders"}, handler)

	if err == nil {
		t.Fatal("a panic in a stream handler produced no error")
	}
	if kind := apperr.KindOf(err); kind != apperr.KindInternal {
		t.Errorf("kind = %v, want Internal", kind)
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
	intercept := Recovery(log)

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
