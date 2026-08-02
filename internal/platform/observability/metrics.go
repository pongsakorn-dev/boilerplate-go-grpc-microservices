package observability

import (
	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics owns the Prometheus registry and the gRPC server metrics.
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
			// Explicit buckets tuned for an RPC service. The defaults top out at 10s,
			// which puts every timeout in the same +Inf bucket and makes "how slow was
			// slow?" unanswerable.
			grpcprom.WithHistogramBuckets([]float64{
				0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30,
			}),
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

	return &Metrics{Registry: reg, Server: server}
}
