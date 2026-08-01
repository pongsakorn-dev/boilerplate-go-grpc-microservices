package interceptor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/platform/apperr"
)

// decodeRecords parses the captured JSON log lines.
func decodeRecords(t *testing.T, raw string) []map[string]any {
	t.Helper()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		out = append(out, rec)
	}
	return out
}

// TestLoggingEmitsExactlyOneRecord pins a deliberate decision.
//
// Emitting a "request started" line as well doubles log volume and cost to say something
// the finish line already implies, and during an incident it doubles what you must read
// past. If this ever becomes two records, it should be because someone decided that -- not
// because an interceptor was added twice to the chain.
func TestLoggingEmitsExactlyOneRecord(t *testing.T) {
	t.Parallel()

	log, buf := captureLogs()
	intercept := Logging(log)

	handler := func(ctx context.Context, req any) (any, error) { return "ok", nil }
	if _, err := intercept(context.Background(), nil, testInfo, handler); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	records := decodeRecords(t, buf.String())
	if len(records) != 1 {
		t.Fatalf("emitted %d records for one RPC, want exactly 1", len(records))
	}

	rec := records[0]
	if rec["grpc.method"] != testInfo.FullMethod {
		t.Errorf("grpc.method = %v, want %s", rec["grpc.method"], testInfo.FullMethod)
	}
	if rec["grpc.code"] != "OK" {
		t.Errorf("grpc.code = %v, want OK", rec["grpc.code"])
	}
	if _, ok := rec["duration_ms"]; !ok {
		t.Error("no duration_ms; latency is invisible in the logs")
	}
}

// TestLoggingLevelsSeparateClientMistakesFromServerFaults is what keeps ERROR meaningful.
//
// A 404 or a validation failure logged at ERROR is noise -- it means a client asked for
// something wrong, which is a normal event in any public API. Log those at ERROR and the
// team learns to ignore ERROR entirely, which is how the one that mattered gets missed.
func TestLoggingLevelsSeparateClientMistakesFromServerFaults(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		err       error
		wantLevel string
		wantCode  string
	}{
		{"not found is the client's problem", apperr.New(apperr.KindNotFound, "R", "m"), "WARN", "NotFound"},
		{"invalid argument is the client's problem", apperr.New(apperr.KindInvalidArgument, "R", "m"), "WARN", "InvalidArgument"},
		{"permission denied is the client's problem", apperr.New(apperr.KindPermissionDenied, "R", "m"), "WARN", "PermissionDenied"},
		{"internal is ours", apperr.New(apperr.KindInternal, "R", "m"), "ERROR", "Internal"},
		{"unavailable is ours", apperr.New(apperr.KindUnavailable, "R", "m"), "ERROR", "Unavailable"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			log, buf := captureLogs()
			intercept := Logging(log)

			// ErrorMap runs INSIDE Logging in the real chain, so by the time logging sees
			// the error it is already a gRPC status. Mirror that here, otherwise the test
			// would assert on a code path production never takes.
			handler := func(ctx context.Context, req any) (any, error) {
				return nil, apperr.ToError(tc.err, "orders")
			}

			_, _ = intercept(context.Background(), nil, testInfo, handler)

			records := decodeRecords(t, buf.String())
			if len(records) != 1 {
				t.Fatalf("got %d records, want 1", len(records))
			}
			if got := records[0]["level"]; got != tc.wantLevel {
				t.Errorf("level = %v, want %v", got, tc.wantLevel)
			}
			if got := records[0]["grpc.code"]; got != tc.wantCode {
				t.Errorf("grpc.code = %v, want %v", got, tc.wantCode)
			}
		})
	}
}

// TestLoggingNeverLogsTheRequestPayload is a privacy control.
//
// Order messages carry customer ids; other services' messages carry worse. Logging bodies
// is how PII ends up in a log aggregator with longer retention than the database it came
// from and none of its access controls.
func TestLoggingNeverLogsTheRequestPayload(t *testing.T) {
	t.Parallel()

	const secret = "customer-ssn-123-45-6789"

	log, buf := captureLogs()
	intercept := Logging(log)

	type payload struct{ CustomerID string }
	req := payload{CustomerID: secret}

	handler := func(ctx context.Context, req any) (any, error) { return "ok", nil }
	_, _ = intercept(context.Background(), req, testInfo, handler)

	if strings.Contains(buf.String(), secret) {
		t.Errorf("the request payload reached the log:\n%s", buf.String())
	}
}

// TestLoggingKeepsTheFullErrorForOperators is the other half of never-leak/never-lose:
// the client gets a redacted message, but the log must keep the cause.
func TestLoggingKeepsTheFullErrorForOperators(t *testing.T) {
	t.Parallel()

	const cause = "dial tcp 10.0.1.5:5432: connect: connection refused"

	log, buf := captureLogs()
	intercept := Logging(log)

	handler := func(ctx context.Context, req any) (any, error) {
		return nil, apperr.Wrap(errors.New(cause), apperr.KindInternal, "DB_DOWN", "database unreachable")
	}
	_, _ = intercept(context.Background(), nil, testInfo, handler)

	if !strings.Contains(buf.String(), "10.0.1.5") {
		t.Errorf("the underlying cause was lost from the log:\n%s", buf.String())
	}
}
