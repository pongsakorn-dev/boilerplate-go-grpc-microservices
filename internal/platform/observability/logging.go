// Package observability owns logging, metrics, tracing and the admin listener.
package observability

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/example/gomicro/internal/platform/config"
)

// NewLogger builds the application logger.
//
// log/slog rather than zap or zerolog: it is in the standard library, so every consumer
// of this template already has it, there is no version to keep current, and
// testing/slogtest can verify a custom handler against the full conformance suite -- which
// is a stronger guarantee than any hand-written handler test.
//
// JSON to stdout by default, because that is what every log shipper expects and because
// writing to a file inside a container is a way to fill a disk.
func NewLogger(cfg config.Config, w io.Writer) *slog.Logger {
	level := parseLevel(cfg.LogLevel)

	opts := &slog.HandlerOptions{
		Level: level,
		// Source is expensive (runtime.CallersFrames per record) and only earns its keep
		// when you are chasing something, so it follows the debug level.
		AddSource: level == slog.LevelDebug,
	}

	var handler slog.Handler
	if strings.EqualFold(cfg.LogFormat, "text") {
		handler = slog.NewTextHandler(w, opts)
	} else {
		handler = slog.NewJSONHandler(w, opts)
	}

	// M3 wraps this in a TraceHandler that injects trace_id and span_id, so a log line
	// and a trace can be correlated without a shared request-id interceptor.
	return slog.New(handler).With(
		slog.String("service", cfg.ServiceName),
		slog.String("version", cfg.Version),
		slog.String("env", cfg.AppEnv),
	)
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

// LevelString renders a level for diagnostics.
func LevelString(l slog.Level) string { return fmt.Sprintf("%v", l) }
