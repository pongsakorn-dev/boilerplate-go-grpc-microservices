package interceptor

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// Logging emits exactly ONE record per RPC, when it finishes.
//
// One record, not two. A "request started" line doubles log volume and cost to tell you
// something the finish line already implies, and during an incident it doubles the noise
// you have to read past. The only thing a start line buys is visibility into requests that
// never finish -- and those are better found through the metrics histogram, which shows
// them as latency, than by diffing start and finish lines.
//
// The request payload is deliberately NOT logged. Order messages contain customer ids;
// other services' messages contain worse. Logging bodies is how PII ends up in a log
// aggregator with a longer retention policy than your database and no access controls.
// What is logged is the method, the outcome, and the duration -- enough to find the
// request, after which the trace has the detail.
//
// trace_id and span_id are added automatically by observability.TraceHandler, so they do
// not appear here.
func Logging(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		record(ctx, log, info.FullMethod, start, err)
		return resp, err
	}
}

// LoggingStream is the streaming equivalent. Duration here is the lifetime of the whole
// stream, which for a long-lived watch is expected to be large -- that is not a latency
// signal and should not be alerted on like one.
func LoggingStream(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		err := handler(srv, ss)
		record(ss.Context(), log, info.FullMethod, start, err)
		return err
	}
}

func record(ctx context.Context, log *slog.Logger, method string, start time.Time, err error) {
	// status.Code reads the FINAL code, which is only correct because ErrorMap sits BELOW
	// this interceptor and has already mapped the domain error. Reverse the two and every
	// classified error is recorded as Unknown. See internal/grpcapi/chain.go.
	code := status.Code(err)

	attrs := []slog.Attr{
		slog.String("grpc.method", method),
		slog.String("grpc.code", code.String()),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
	}

	level := slog.LevelInfo
	if err != nil {
		// The full error, including the wrapped cause, goes to the log even though the
		// client only ever sees the redacted message. Never leak, never lose.
		attrs = append(attrs, slog.String("error", err.Error()))

		// Client mistakes are not server problems. A 404 or a validation failure logged at
		// ERROR trains everyone to ignore ERROR, which is how the real one gets missed.
		if isServerFault(code) {
			level = slog.LevelError
		} else {
			level = slog.LevelWarn
		}
	}

	log.LogAttrs(ctx, level, "rpc finished", attrs...)
}
