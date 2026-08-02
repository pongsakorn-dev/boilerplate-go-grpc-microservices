package observability_test

import (
	"context"
	"net"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/platform/observability"
	"github.com/example/gomicro/internal/testutil"
)

// FilterHealthChecks shipped with no test at all, and it is the kind of code that fails
// silently in the expensive direction: if the filter stops being applied, nothing breaks,
// no alert fires, and the only symptom is a tracing bill and a UI in which every real trace
// is buried under probe noise.
//
// The number in tracing.go's comment is what makes this worth a test rather than a code
// review: Kubernetes runs liveness, readiness and startup probes at roughly one second each,
// so a three-replica deployment emits on the order of 780,000 health-check spans a day.
//
// These tests drive the PRODUCTION server through testutil.NewTestServer, so what is under
// assertion is the filter as chain.go actually installs it -- not the predicate in isolation.
// A unit test of FilterHealthChecks would pass with the otelgrpc.WithFilter option deleted.

// discardOTLPSink accepts OTLP connections and refuses the export, quickly.
//
// A DEAD ADDRESS IS THE OBVIOUS CHOICE AND IT COSTS FIVE SECONDS PER TEST. Connection refused
// is retryable, so otlptracegrpc backs off and retries until the export timeout, and the whole
// delay is paid during app.Close where it looks like a slow shutdown rather than a slow
// exporter. A listener that ACCEPTS and answers Unimplemented ends the export on the first
// attempt, because Unimplemented is not in the OTLP retryable set. Measured: 5.0s per test
// against a refused port, 0.02s against this.
func discardOTLPSink(t *testing.T) string {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the OTLP sink: %v", err)
	}

	// A gRPC server with nothing registered answers every method Unimplemented, which is
	// exactly the fast non-retryable rejection this needs.
	srv := grpc.NewServer()
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return lis.Addr().String()
}

// newRecordingServer starts the production server with a real (non-noop) tracer provider and
// returns both a client and the recorder collecting its spans.
//
// GETTING A RECORDER IN IS THE AWKWARD PART, and it is why an endpoint is configured at all.
// observability.NewTracerProvider returns a no-op provider whenever OTEL_EXPORTER_OTLP_ENDPOINT
// is empty -- the right production default, and what makes a fresh clone run with no collector
// -- but it means the obvious test records nothing and passes vacuously. Setting an endpoint
// takes the branch that builds a real SDK provider.
//
// The recorder is then attached as an ADDITIONAL span processor on that same provider. It has
// to be the same instance: otelgrpc captures otel.GetTracerProvider() when the handler is
// constructed inside app.New, so replacing the global afterwards would leave the server
// reporting to the provider it captured and this test watching an empty one.
func newRecordingServer(t *testing.T) (orderv1.OrderServiceClient, healthpb.HealthClient, *tracetest.SpanRecorder) {
	t.Helper()

	// The export is expected to fail; the recorder is what this test reads. Without this,
	// every run prints an exporter error that looks like a broken test.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {}))

	endpoint := discardOTLPSink(t)

	conn := testutil.NewTestServer(t, func(c *config.Config) {
		c.Telemetry.OTLPEndpoint = endpoint
		c.Telemetry.TraceSampleRatio = 1.0
	})

	tp, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider)
	if !ok {
		t.Fatalf("the global tracer provider is %T, not an SDK provider.\n\n"+
			"NewTracerProvider took its no-op branch, so no span would be recorded no matter "+
			"what the server does and every assertion here would pass vacuously.",
			otel.GetTracerProvider())
	}

	recorder := tracetest.NewSpanRecorder()
	tp.RegisterSpanProcessor(recorder)
	t.Cleanup(func() { tp.UnregisterSpanProcessor(recorder) })

	return orderv1.NewOrderServiceClient(conn), healthpb.NewHealthClient(conn), recorder
}

// TestHealthChecksProduceNoSpans is the assertion the filter exists for.
//
// Not parallel, and it cannot be: otelgrpc resolves the tracer provider from a process-global,
// so two tests swapping providers concurrently would sample each other's spans.
func TestHealthChecksProduceNoSpans(t *testing.T) {
	orders, healthClient, recorder := newRecordingServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// THE CONTROL COMES FIRST, and it is doing real work here rather than being a formality.
	//
	// "Zero health-check spans" is also what a completely broken tracing pipeline produces --
	// a no-op provider, an unsampled trace, a stats handler that was never installed. Proving
	// an ordinary RPC DOES produce a span first is what makes the zero below mean "filtered"
	// instead of "nothing is being traced at all".
	if _, err := orders.ListOrders(ctx, &orderv1.ListOrdersRequest{}); err != nil {
		t.Fatalf("ListOrders: %v", err)
	}
	if got := len(recorder.Ended()); got == 0 {
		t.Fatal("an ordinary RPC produced no spans.\n\n" +
			"Tracing is not running at all, so the health-check assertion below would pass " +
			"against a server that traces nothing.")
	}

	before := len(recorder.Ended())

	// Ten probes: roughly ten seconds of one pod's liveness checks.
	for range 10 {
		if _, err := healthClient.Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
			t.Fatalf("health check: %v", err)
		}
	}

	if got := len(recorder.Ended()) - before; got != 0 {
		t.Errorf("10 health checks produced %d spans, want 0.\n\n"+
			"The otelgrpc.WithFilter option in chain.go is not being applied. At a 1s probe "+
			"interval across three probes and three replicas that is roughly 780,000 spans "+
			"a day carrying no information, which buries real traces in the UI and is billed "+
			"per span by every managed tracing backend.", got)
	}
}

// TestAnOrdinaryRPCIsTracedWithTheServiceName checks the other half: the filter must not be
// so broad that it silences the traffic tracing exists for.
//
// A filter written as `return false` would pass TestHealthChecksProduceNoSpans perfectly.
func TestAnOrdinaryRPCIsTracedWithTheServiceName(t *testing.T) {
	orders, _, recorder := newRecordingServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := orders.ListOrders(ctx, &orderv1.ListOrdersRequest{}); err != nil {
		t.Fatalf("ListOrders: %v", err)
	}

	spans := recorder.Ended()
	if len(spans) == 0 {
		t.Fatal("ListOrders produced no spans")
	}

	// The span must name the method. A span called something generic is not usable for the
	// thing traces are opened for -- finding which RPC was slow.
	var found bool
	for _, s := range spans {
		if s.Name() == "order.v1.OrderService/ListOrders" {
			found = true
			break
		}
	}
	if !found {
		names := make([]string, 0, len(spans))
		for _, s := range spans {
			names = append(names, s.Name())
		}
		t.Errorf("no span is named for the RPC; got %v", names)
	}
}

// TestFilterHealthChecksNamesExactlyTheProbeMethod covers the predicate itself.
//
// Cheap, and it pins the one thing the server-level tests cannot distinguish: WHICH method is
// excluded. A filter that also dropped, say, everything under /grpc. would pass both tests
// above while silently untracing reflection and any future infrastructure service.
func TestFilterHealthChecksNamesExactlyTheProbeMethod(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		method string
		traced bool
	}{
		{observability.HealthCheckMethod, false},
		{"/order.v1.OrderService/ListOrders", true},
		{"/order.v1.OrderService/CreateOrder", true},

		// Watch is the streaming health API. It is NOT filtered, and that is deliberate
		// rather than an oversight: nothing probes it on an interval, so it produces one
		// span per subscription rather than one per second.
		{"/grpc.health.v1.Health/Watch", true},

		// Reflection stays traced. It is a real client action worth seeing in a trace.
		{"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo", true},
	} {
		if got := observability.FilterHealthChecks(tc.method); got != tc.traced {
			t.Errorf("FilterHealthChecks(%q) = %v, want %v", tc.method, got, tc.traced)
		}
	}
}
