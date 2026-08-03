package observability

import (
	"sync"

	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// latencyBuckets is shared by the server and every client, and the sharing is the point.
//
// Client and server latency for the same hop are only comparable in PromQL if their histograms
// agree: histogram_quantile over two different bucket layouts is not a comparison, it is two
// unrelated numbers on one graph. The most useful thing these metrics can tell you is the gap
// between "how long the caller waited" and "how long the callee worked" -- that difference IS
// the network and the queue -- and a second bucket list quietly makes it unanswerable.
//
// The values are tuned for an RPC service: the library default tops out at 10s, which puts
// every timeout in the same +Inf bucket and makes "how slow was slow?" unanswerable.
var latencyBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30,
}

// Metrics owns the Prometheus registry and the gRPC metrics.
type Metrics struct {
	// Registry is a DEDICATED registry, never prometheus.DefaultRegisterer.
	//
	// The default registerer is process-global mutable state. Any dependency, anywhere in
	// the tree, can register a collector into it from an init() function -- so what your
	// /metrics endpoint exposes stops being something you decided. It also makes tests
	// order-dependent, because a duplicate registration panics.
	//
	// An explicit registry means the exposed metric set is exactly what this file says it
	// is, and a test can build a fresh pedantic registry per case with no global state.
	Registry *prometheus.Registry

	// Server instruments every RPC: request counts by code, and latency histograms.
	Server *grpcprom.ServerMetrics

	// clients holds one ClientMetrics per upstream. See ClientFor.
	mu      sync.Mutex
	clients map[string]*grpcprom.ClientMetrics
}

// NewProcessRegistry builds a registry carrying only the runtime and process collectors.
//
// For a binary that serves no RPCs -- cmd/worker, which opens no gRPC listener at all. Giving
// it the full Metrics would publish a complete set of grpc_server_* series that can never move
// off zero, and a metric that is structurally incapable of changing is worse than an absent
// one: it invites dashboards and alerts built on a number that will never fire.
func NewProcessRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}

// NewMetrics builds the registry and registers the standard collectors.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()

	server := grpcprom.NewServerMetrics(
		// Histograms, not just counters.
		//
		// A latency AVERAGE is close to useless for a service: it hides the tail, and the
		// tail is what users experience. Histograms are what make p99 computable in
		// PromQL.
		grpcprom.WithServerHandlingTimeHistogram(
			grpcprom.WithHistogramBuckets(latencyBuckets),
		),
	)

	reg.MustRegister(
		server,
		// Go runtime metrics: goroutines, GC pauses, heap. The first thing anyone wants
		// during an incident, and free.
		collectors.NewGoCollector(),
		// Process metrics: CPU, RSS, open file descriptors. FD exhaustion in particular is
		// invisible until it takes the process down.
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return &Metrics{Registry: reg, Server: server, clients: map[string]*grpcprom.ClientMetrics{}}
}

// ClientFor returns the grpc_client_* collectors for one upstream, creating them on first use.
//
// ONE PER UPSTREAM, NOT ONE PER PROCESS, and that is the whole reason this is a method rather
// than a field.
//
// grpcprom's client metrics are labelled by service and method and NOTHING ELSE -- there is no
// label for which upstream was called. A single shared ClientMetrics therefore sums every
// upstream into one series, so two dependencies with the same method name are indistinguishable
// and "which upstream got slow?" -- the only question these metrics exist to answer -- has no
// answer. It is not a limitation you discover from the data, either: the graph looks fine, it
// is just wrong.
//
// The fix is a constant label per upstream, applied by wrapping the registerer. Each upstream
// gets its own ClientMetrics whose series carry upstream="name", and PromQL can sum across them
// when you want the total.
//
// MEMOISED BY NAME because prometheus.MustRegister PANICS on a duplicate. Two connections to
// the same upstream -- entirely reasonable -- would otherwise take the process down at startup.
// Same name returns the same instance and registers nothing new; the metrics are shared, which
// is exactly right for two connections to the same place.
func (m *Metrics) ClientFor(upstream string) *grpcprom.ClientMetrics {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.clients == nil {
		m.clients = map[string]*grpcprom.ClientMetrics{}
	}
	if existing, ok := m.clients[upstream]; ok {
		return existing
	}

	client := grpcprom.NewClientMetrics(
		// The SAME buckets as the server. See latencyBuckets.
		grpcprom.WithClientHandlingTimeHistogram(
			grpcprom.WithHistogramBuckets(latencyBuckets),
		),
	)

	// WrapRegistererWith adds the label to every series this collector publishes, without
	// grpcprom needing to know the label exists.
	prometheus.WrapRegistererWith(prometheus.Labels{"upstream": upstream}, m.Registry).
		MustRegister(client)

	m.clients[upstream] = client
	return client
}

// UPSTREAM SERIES DO NOT EXIST UNTIL THE FIRST CALL, and unlike the server side that cannot be
// fixed here.
//
// NewServer calls Server.InitializeMetrics(srv) so every registered method has a zero-valued
// series from startup -- without it, a PromQL alert on a method's error rate evaluates to "no
// data" rather than zero and never fires. The client has no equivalent: grpcprom cannot
// enumerate the methods a client might call, because nothing declares them.
//
// So an alert on grpc_client_handled_total for a method that has never been called will not
// fire. Write client alerts with absent() or over a recording rule with a default, not on the
// raw series.
