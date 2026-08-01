package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"testing/slogtest"

	"go.opentelemetry.io/otel/trace"

	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/platform/observability"
)

// TestTraceHandlerPassesTheStdlibConformanceSuite is worth far more than any hand-written
// handler test.
//
// slog.Handler has a surprising amount of required behaviour -- how WithGroup nests
// subsequent attributes, that empty groups are elided, that an empty Attr is dropped, that
// WithAttrs must not mutate the receiver. A wrapping handler gets these wrong by omission,
// and the symptom is malformed JSON in production logs rather than a test failure.
//
// slogtest exercises all of it, so wrapping a compliant handler stays compliant.
func TestTraceHandlerPassesTheStdlibConformanceSuite(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	handler := observability.NewTraceHandler(slog.NewJSONHandler(&buf, nil))

	results := func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("handler emitted invalid JSON: %v", err)
			}
			out = append(out, m)
		}
		return out
	}

	if err := slogtest.TestHandler(handler, results); err != nil {
		t.Errorf("TraceHandler is not a conformant slog.Handler:\n%v", err)
	}
}

// TestTraceHandlerInjectsTraceContext is the feature itself.
func TestTraceHandlerInjectsTraceContext(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(observability.NewTraceHandler(slog.NewJSONHandler(&buf, nil)))

	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	logger.InfoContext(ctx, "handling request")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if rec["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace_id = %v, want the span's trace id", rec["trace_id"])
	}
	if rec["span_id"] != "00f067aa0ba902b7" {
		t.Errorf("span_id = %v, want the span's id", rec["span_id"])
	}
}

// TestTraceHandlerSurvivesWith is the subtle bug this design is most likely to have.
//
// logger.With(...) is used everywhere -- NewLogger itself calls it to attach service and
// version. If WithAttrs returned the INNER handler instead of a re-wrapped TraceHandler,
// every derived logger would silently stop emitting trace_id. Nothing would error;
// correlation would just quietly stop working on exactly the loggers the application uses.
func TestTraceHandlerSurvivesWith(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	base := slog.New(observability.NewTraceHandler(slog.NewJSONHandler(&buf, nil)))

	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(
		trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled}))

	derived := base.With(slog.String("service", "orderd")).WithGroup("req")
	derived.InfoContext(ctx, "still correlated")

	if !strings.Contains(buf.String(), "4bf92f3577b34da6a3ce929d0e0e4736") {
		t.Errorf("a derived logger lost trace correlation:\n%s", buf.String())
	}
}

// TestNoTraceContextIsNotAnError: most logs (startup, shutdown, background work) have no
// span. They must log normally rather than emitting empty trace fields that pollute every
// index in the log store.
func TestNoTraceContextIsNotAnError(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(observability.NewTraceHandler(slog.NewJSONHandler(&buf, nil)))

	logger.InfoContext(context.Background(), "starting up")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := rec["trace_id"]; ok {
		t.Error("an empty trace_id was emitted; it should be omitted entirely")
	}
	if rec["msg"] != "starting up" {
		t.Errorf("msg = %v, want the message to be logged normally", rec["msg"])
	}
}

func TestNewLoggerRespectsLevelAndFormat(t *testing.T) {
	t.Parallel()

	t.Run("level filters records", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		cfg := testConfig(t, map[string]string{"LOG_LEVEL": "warn"})
		logger := observability.NewLogger(cfg, &buf)

		logger.Info("should be filtered")
		logger.Warn("should appear")

		if strings.Contains(buf.String(), "should be filtered") {
			t.Error("an info record survived LOG_LEVEL=warn")
		}
		if !strings.Contains(buf.String(), "should appear") {
			t.Error("a warn record was filtered at LOG_LEVEL=warn")
		}
	})

	t.Run("service identity is on every record", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		logger := observability.NewLogger(testConfig(t, nil), &buf)

		logger.Info("hello")

		var rec map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		// Without these, logs from three services in one aggregator are indistinguishable.
		for _, key := range []string{"service", "version", "env"} {
			if _, ok := rec[key]; !ok {
				t.Errorf("record has no %q field", key)
			}
		}
	})
}

func testConfig(t *testing.T, overrides map[string]string) config.Config {
	t.Helper()

	env := map[string]string{"SERVICE_NAME": "orderd-test"}
	for k, v := range overrides {
		env[k] = v
	}
	cfg, err := config.Parse(env)
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	return cfg
}
