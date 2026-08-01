// Package observability owns logging, metrics, tracing and the admin listener.
package observability

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/example/gomicro/internal/platform/config"
)

// NewLogger builds the application logger.
//
// log/slog rather than zap or zerolog: it is in the standard library, so every consumer of
// this template already has it, there is no version to keep current, and testing/slogtest
// can verify a custom handler against the full conformance suite -- a stronger guarantee
// than any hand-written handler test.
//
// JSON to stdout by default, because that is what every log shipper expects and because
// writing to a file inside a container is a way to fill a disk.
func NewLogger(cfg config.Config, w io.Writer) *slog.Logger {
	level := parseLevel(cfg.LogLevel)

	opts := &slog.HandlerOptions{
		Level: level,
		// Source resolution costs a runtime.CallersFrames walk per record, and only earns
		// it while you are actively chasing something. Tie it to the debug level.
		AddSource: level == slog.LevelDebug,
	}

	var base slog.Handler
	if strings.EqualFold(cfg.LogFormat, "text") {
		base = slog.NewTextHandler(w, opts)
	} else {
		base = slog.NewJSONHandler(w, opts)
	}

	return slog.New(NewTraceHandler(base)).With(
		slog.String("service", cfg.ServiceName),
		slog.String("version", cfg.Version),
		slog.String("env", cfg.AppEnv),
	)
}

// TraceHandler injects trace_id and span_id into every record logged with a context.
//
// This is what makes logs and traces one dataset: paste a trace_id from a log line into
// Jaeger and get the whole request; click a slow span and grep its trace_id for the logs.
//
// It also removes the need for a request-id interceptor entirely. The OTel trace id IS the
// correlation id -- it is already propagated across service boundaries by W3C traceparent,
// already unique, and cannot be spoofed by a client header. Shipping a separate request-id
// means an extra interceptor, extra tests, and an attacker-controlled metadata value that
// ends up in your logs (4KB of it, possibly containing newlines, i.e. log injection).
type TraceHandler struct {
	inner slog.Handler
}

// NewTraceHandler wraps a handler.
func NewTraceHandler(inner slog.Handler) *TraceHandler {
	return &TraceHandler{inner: inner}
}

func (h *TraceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *TraceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs and WithGroup must re-wrap.
//
// Returning the inner handler here would be a subtle and total failure: logger.With(...)
// is used everywhere, and any logger derived from one would silently stop emitting
// trace_id. Nothing would error -- correlation would just quietly stop working on the
// loggers that matter most. stdlib slogtest catches exactly this class of bug, which is
// why logging_test.go runs the full conformance suite rather than one hand-written case.
func (h *TraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TraceHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *TraceHandler) WithGroup(name string) slog.Handler {
	return &TraceHandler{inner: h.inner.WithGroup(name)}
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
