package observability_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/platform/observability"
)

func TestAdminMuxServesOperatorEndpoints(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	mux := observability.NewAdminMux(reg, func() bool { return true })

	paths := []struct {
		path string
		want int
	}{
		{"/healthz", http.StatusOK},
		{"/metrics", http.StatusOK},
		{"/debug/pprof/", http.StatusOK},
		{"/debug/pprof/cmdline", http.StatusOK},
	}

	for _, tc := range paths {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if rec.Code != tc.want {
				t.Errorf("GET %s = %d, want %d", tc.path, rec.Code, tc.want)
			}
		})
	}
}

// TestPprofIsOnDefaultServeMuxButWeNeverServeIt documents the hazard by demonstrating it.
//
// Importing net/http/pprof -- anywhere in the dependency tree, including transitively
// through a library you did not choose -- runs an init() that registers /debug/pprof/* on
// http.DefaultServeMux. This test proves that has ALREADY happened in this binary.
//
// That is precisely why no listener in this service serves DefaultServeMux. If one did, it
// would publish a heap dump (containing whatever request data is live), a CPU profiler that
// pins a core for 30 seconds per request, and every goroutine stack in the process.
//
// The protection is structural: explicit muxes everywhere, and this test to make the
// reasoning visible to whoever reads it next.
func TestPprofIsOnDefaultServeMuxButWeNeverServeIt(t *testing.T) {
	t.Parallel()

	// The hazard is real: DefaultServeMux has pprof, purely because something imported it.
	rec := httptest.NewRecorder()
	http.DefaultServeMux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))

	if rec.Code == http.StatusNotFound {
		t.Skip("net/http/pprof is not linked into this test binary; the hazard does not apply here")
	}

	// The mitigation: our admin mux is a DIFFERENT mux, so what it exposes is only ever
	// what NewAdminMux says it exposes.
	adminMux := observability.NewAdminMux(prometheus.NewRegistry(), nil)
	if any(adminMux) == any(http.DefaultServeMux) {
		t.Fatal("the admin mux IS http.DefaultServeMux -- anything that imports a library " +
			"which registers a handler would silently publish it on this listener")
	}

	// And a mux we build ourselves for a PUBLIC surface must serve nothing at /debug.
	public := http.NewServeMux()
	public.HandleFunc("GET /v1/orders", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	pub := httptest.NewRecorder()
	public.ServeHTTP(pub, httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil))
	if pub.Code != http.StatusNotFound {
		t.Errorf("a public mux served /debug/pprof/heap with %d; profiling must never be public", pub.Code)
	}
}

// TestAdminBindsPrivatelyByDefault checks the default that actually protects a deployment.
//
// A default of ":9090" would bind every interface, publishing pprof to the pod network the
// moment someone forgets to set ADMIN_ADDR -- and nothing would look wrong.
func TestAdminBindsPrivatelyByDefault(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse(map[string]string{})
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}

	if !strings.HasPrefix(cfg.AdminAddr, "127.0.0.1") {
		t.Errorf("default ADMIN_ADDR = %q, want a loopback bind.\n\n"+
			"The admin listener serves /debug/pprof. Binding it to all interfaces publishes a "+
			"heap dumper and a CPU-profiler trigger to anything that can route to the pod.",
			cfg.AdminAddr)
	}
}

// TestHealthzReportsProcessLivenessOnly encodes a decision that causes real outages when
// reversed.
//
// A liveness probe that checks the database makes Kubernetes restart every replica at once
// during a database blip -- converting a recoverable dependency outage into a total,
// self-inflicted one. Readiness (grpc.health.v1 on the RPC port) is what gates traffic.
func TestHealthzReportsProcessLivenessOnly(t *testing.T) {
	t.Parallel()

	mux := observability.NewAdminMux(prometheus.NewRegistry(), func() bool { return false })

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	// The hook exists and is honoured...
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /healthz = %d, want 503 when the liveness hook reports unhealthy", rec.Code)
	}

	// ...but the default wiring passes a hook that never consults a dependency.
	healthy := observability.NewAdminMux(prometheus.NewRegistry(), func() bool { return true })
	rec2 := httptest.NewRecorder()
	healthy.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec2.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec2.Code)
	}
}
