package observability

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/example/gomicro/internal/platform/config"
)

// HealthCheckMethod is filtered out of tracing.
//
// Kubernetes probes this on a 1-second interval, from three probes, on every pod. At three
// replicas that is roughly a quarter of a million spans per day that describe nothing but
// "the pod is still alive" -- they bury real traces in the UI and cost real money in every
// managed tracing backend.
const HealthCheckMethod = "/grpc.health.v1.Health/Check"

// NewTracerProvider builds the trace pipeline.
//
// When no OTLP endpoint is configured it returns a no-op provider rather than failing.
// That is what lets `go run ./cmd/orderd` work on a fresh clone with no collector: tracing
// is instrumented everywhere, and simply goes nowhere until you point it at something.
func NewTracerProvider(ctx context.Context, cfg config.Config) (trace.TracerProvider, func(context.Context) error, error) {
	// Propagation is installed unconditionally, even with no exporter.
	//
	// It costs nothing and it is what makes trace context flow ACROSS services. If this
	// service does not propagate, a downstream service that does export produces orphaned
	// traces -- and the gap looks like the downstream team's bug.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, // W3C traceparent
		propagation.Baggage{},
	))

	if cfg.Telemetry.OTLPEndpoint == "" {
		tp := noop.NewTracerProvider()
		otel.SetTracerProvider(tp)
		return tp, func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Telemetry.OTLPEndpoint),
		otlptracegrpc.WithInsecure(), // the collector is normally a sidecar or in-cluster
	)
	if err != nil {
		return nil, nil, fmt.Errorf("otlp trace exporter: %w", err)
	}

	res, err := newResource(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build otel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithMaxQueueSize(2048),
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithResource(res),

		// ParentBased wrapping the ratio sampler, not the ratio sampler alone.
		//
		// This is the difference between usable and useless traces. A bare ratio sampler
		// decides independently at every hop, so with a 10% ratio a three-service request
		// has a 0.1% chance of being sampled end to end -- you get fragments of traces and
		// almost never a whole one. ParentBased honours the caller's decision, so a trace
		// that starts sampled stays sampled through every service.
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(cfg.Telemetry.TraceSampleRatio),
		)),
	)

	otel.SetTracerProvider(tp)

	return tp, func(ctx context.Context) error {
		// Shutdown flushes whatever is still queued. Without it, the last few seconds of
		// traces before a deploy -- often the interesting ones -- are silently dropped.
		return tp.Shutdown(ctx)
	}, nil
}

// newResource describes THIS service to the tracing backend.
//
// Merged with resource.Default() so the SDK's own attributes (telemetry.sdk.*) survive.
// NewSchemaless avoids a schema-URL conflict with Default(): merging a schema'd resource
// with a differently-schema'd one is an error, and that error would take down startup for
// a reason that has nothing to do with the service.
func newResource(cfg config.Config) (*resource.Resource, error) {
	return resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.Version),
			// Written literally rather than via a semconv helper: the attribute was
			// renamed across semconv versions, and pinning the string keeps this
			// compiling through an SDK upgrade.
			attribute.String("deployment.environment.name", cfg.AppEnv),
		),
	)
}

// FilterHealthChecks reports whether a gRPC method should be traced.
//
// Used as the otelgrpc filter. Kept as a named function rather than an inline closure so a
// test can assert on it directly without constructing a server.
func FilterHealthChecks(fullMethod string) bool {
	return fullMethod != HealthCheckMethod
}

// TraceIDFrom returns the current trace id, or "" when there is no recording span.
func TraceIDFrom(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// SpanIDFrom returns the current span id, or "".
func SpanIDFrom(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.SpanID().String()
}
