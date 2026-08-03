package client_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
	"github.com/example/gomicro/internal/platform/client"
	"github.com/example/gomicro/internal/platform/observability"
)

// The outbound client shipped with no metrics at all, and the gap had a specific cost: you
// could see that this service was slow and not that its upstream was. That is the most common
// wasted hour of an incident, and the data to end it is two interceptors away.
//
// These tests are about the SHAPE of what is published rather than the counting, which
// grpcprom already does correctly. The shape is where this goes wrong, silently, in a way no
// dashboard reveals.

// TestUpstreamsDoNotCollapseIntoOneSeries is the assertion the whole ClientFor design exists
// for.
//
// grpcprom labels client series by service and method and nothing else -- there is no label
// naming which upstream was called. Share one ClientMetrics across two upstreams and both sum
// into the same series, so two dependencies that happen to expose a method of the same name
// become indistinguishable and "which upstream got slow?" has no answer.
//
// The failure is invisible from the data: the graph looks perfectly healthy, it is just adding
// up two different things. A test is the only place it can be caught.
func TestUpstreamsDoNotCollapseIntoOneSeries(t *testing.T) {
	t.Parallel()

	metrics := observability.NewMetrics()

	// Two upstreams, one shared Metrics -- the realistic arrangement.
	callUpstream(t, metrics, "passthrough:///orders")
	callUpstream(t, metrics, "passthrough:///inventory")

	exposition := gather(t, metrics.Registry, "grpc_client_handled_total")

	for _, want := range []string{`upstream="passthrough:///orders"`, `upstream="passthrough:///inventory"`} {
		if !strings.Contains(exposition, want) {
			t.Errorf("no client series carries %s.\n\n"+
				"Both upstreams are summed into one set of series, so the metrics cannot say "+
				"which dependency is failing -- which is the only question they exist to "+
				"answer.\n\ngot:\n%s", want, exposition)
		}
	}
}

// TestDialingTheSameUpstreamTwiceDoesNotPanic covers the failure mode that would take the
// process down at startup rather than degrading anything.
//
// prometheus.MustRegister PANICS on a duplicate collector. Registering a fresh ClientMetrics
// per Dial would therefore kill any service that opens two connections to the same upstream --
// a perfectly ordinary thing to do, and a crash that happens at boot on the day someone adds
// the second connection.
func TestDialingTheSameUpstreamTwiceDoesNotPanic(t *testing.T) {
	t.Parallel()

	metrics := observability.NewMetrics()

	callUpstream(t, metrics, "passthrough:///orders")
	callUpstream(t, metrics, "passthrough:///orders") // would panic on a duplicate registration

	// And they share one collector rather than publishing two conflicting sets.
	if got := strings.Count(gather(t, metrics.Registry, "grpc_client_started_total"),
		`upstream="passthrough:///orders"`); got == 0 {
		t.Error("the second dial produced no series at all")
	}
}

// TestClientAndServerHistogramsShareTheirBuckets is why latencyBuckets is one variable.
//
// The most useful thing this pair of metrics can say is the DIFFERENCE between how long the
// caller waited and how long the callee worked -- that gap is the network and the queue.
// histogram_quantile over two different bucket layouts is not a comparison, though; it is two
// unrelated numbers drawn on one graph, and nothing about the graph says so.
func TestClientAndServerHistogramsShareTheirBuckets(t *testing.T) {
	t.Parallel()

	metrics := observability.NewMetrics()

	// Server side. InitializeMetrics is what NewServer calls after registration, and it
	// preallocates a zero-valued series per method -- which is what carries the bucket layout
	// before any request has been served.
	srv := grpc.NewServer()
	orderv1.RegisterOrderServiceServer(srv, &orderv1.UnimplementedOrderServiceServer{})
	metrics.Server.InitializeMetrics(srv)

	// Client side, through the same shared Metrics.
	callUpstream(t, metrics, "passthrough:///orders")

	clientBuckets := histogramBuckets(t, metrics.Registry, "grpc_client_handling_seconds")
	serverBuckets := histogramBuckets(t, metrics.Registry, "grpc_server_handling_seconds")

	if clientBuckets == "" {
		t.Fatal("no client latency histogram was published")
	}
	if serverBuckets == "" {
		t.Fatal("no server latency histogram was published; the comparison would be vacuous")
	}
	if clientBuckets != serverBuckets {
		t.Errorf("client and server latency histograms use different buckets.\n\n"+
			"client: %s\nserver: %s\n\n"+
			"They are not comparable in one PromQL expression, so the gap between caller wait "+
			"and callee work -- the number worth having -- cannot be computed.",
			clientBuckets, serverBuckets)
	}
}

// TestNilMetricsIsWorkable keeps the harness cheap.
//
// Tests build connections by the dozen; requiring a registry for each would be noise, and a
// nil-means-panic here would make the client unusable in exactly the places it is most tested.
func TestNilMetricsIsWorkable(t *testing.T) {
	t.Parallel()

	conn, _ := dialRealServer(t) // Options.Metrics left nil
	if _, err := orderv1.NewOrderServiceClient(conn).ListOrders(
		context.Background(), &orderv1.ListOrdersRequest{}); err != nil {
		t.Fatalf("a client with no Metrics could not make a call: %v", err)
	}
}

// --- helpers ---

// callUpstream dials the production server under a given target name and makes one call, so
// grpcprom has something to record.
func callUpstream(t *testing.T, metrics *observability.Metrics, target string) {
	t.Helper()

	conn, _ := dialRealServer(t, func(o *client.Options) {
		o.Target = target
		o.Metrics = metrics
	})

	// The call itself may fail -- the point is that it was OBSERVED, and grpcprom records
	// handled_total for a failure just as it does for a success.
	_, _ = orderv1.NewOrderServiceClient(conn).ListOrders(
		context.Background(), &orderv1.ListOrdersRequest{})
}

// gather renders the registry as Prometheus text, which is what a scrape would see.
//
// Asserting on the exposition rather than on collector internals: a label that exists in Go
// and not in the output is worth nothing, and the metric NAME is part of the contract that
// alerts are written against.
func gather(t *testing.T, reg prometheus.Gatherer, only string) string {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	var b strings.Builder
	for _, f := range families {
		if only != "" && !strings.HasPrefix(f.GetName(), only) {
			continue
		}
		for _, m := range f.GetMetric() {
			b.WriteString(f.GetName())
			b.WriteString("{")
			for i, l := range m.GetLabel() {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(l.GetName() + `="` + l.GetValue() + `"`)
			}
			b.WriteString("}\n")
		}
	}
	return b.String()
}

// histogramBuckets returns one histogram's upper bounds, as a comparable string.
//
// Read from the gathered histogram body rather than from the text exposition: bucket bounds
// live in the metric's Bucket list, not in its labels, so a label-based reader finds nothing
// and reports "no histogram" for a histogram that is right there. That mistake cost a test run.
func histogramBuckets(t *testing.T, reg prometheus.Gatherer, name string) string {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			h := m.GetHistogram()
			if h == nil {
				continue
			}
			bounds := make([]string, 0, len(h.GetBucket()))
			for _, b := range h.GetBucket() {
				bounds = append(bounds, strconv.FormatFloat(b.GetUpperBound(), 'g', -1, 64))
			}
			return strings.Join(bounds, " ")
		}
	}
	return ""
}
