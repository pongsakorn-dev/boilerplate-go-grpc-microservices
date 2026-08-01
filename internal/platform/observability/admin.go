package observability

import (
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/example/gomicro/internal/platform/config"
)

// NewAdminMux builds the private operator surface: metrics, profiling, and a liveness
// endpoint.
//
// Everything here is registered on an EXPLICIT mux, never http.DefaultServeMux, and the
// server that serves it binds to a private address.
//
// That is a security control, not tidiness. Importing net/http/pprof -- anywhere in the
// dependency tree, including transitively through a library you did not choose -- runs an
// init() that registers /debug/pprof/* on http.DefaultServeMux. If any public listener
// ever serves DefaultServeMux, you have published:
//
//   - /debug/pprof/heap, a full heap dump containing whatever request data is live
//   - /debug/pprof/profile, which pins a CPU for 30 seconds per request (a free DoS)
//   - /debug/pprof/goroutine?debug=2, every stack trace in the process
//
// Using our own mux makes that impossible by construction rather than by discipline.
// admin_test.go asserts the public listeners do not serve these paths.
func NewAdminMux(reg *prometheus.Registry, live func() bool) *http.ServeMux {
	mux := http.NewServeMux()

	// Liveness: is the process wedged? This must NOT check dependencies.
	//
	// A liveness probe that returns unhealthy when the database is down causes Kubernetes
	// to restart every replica simultaneously during a database blip -- turning a
	// recoverable dependency outage into a full outage of your own making. Readiness is
	// what gates traffic, and readiness here is grpc.health.v1 on the RPC port.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if live != nil && !live() {
			http.Error(w, "not live", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		// Surface scrape-time errors instead of silently serving a partial exposition.
		ErrorHandling: promhttp.HTTPErrorOnError,
	}))

	// pprof handlers, registered EXPLICITLY.
	//
	// Referencing pprof.Index and friends by name is what keeps this honest: it documents
	// that profiling is deliberately exposed here and nowhere else.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	return mux
}

// NewAdminServer wraps the admin mux in a server with sane timeouts.
func NewAdminServer(cfg config.Config, mux *http.ServeMux) *http.Server {
	return &http.Server{
		Addr:    cfg.AdminAddr,
		Handler: mux,

		// ReadHeaderTimeout closes the Slowloris hole: without it, a client can hold a
		// connection open indefinitely by dribbling headers one byte at a time.
		ReadHeaderTimeout: 5 * time.Second,

		// No WriteTimeout on purpose. /debug/pprof/profile streams for 30 seconds by
		// default and /debug/pprof/trace can run longer; a WriteTimeout would truncate
		// exactly the profile you are trying to capture during an incident.
		ReadTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second,
	}
}
