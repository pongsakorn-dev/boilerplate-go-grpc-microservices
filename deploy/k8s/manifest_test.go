// Package k8s exists only to hold this test.
//
// The manifests are YAML with no Go in them, so a package here is slightly unusual -- but the
// alternative is that nothing in `go test ./...` ever reads them, and deployment YAML is
// exactly the kind of file that rots silently because it is only exercised by a cluster
// nobody has locally.
//
// WHY yaml.v3 AND NOT krusty. The obvious implementation builds the overlays with
// sigs.k8s.io/kustomize/api as a library, which would also verify that base and overlay
// compose. It was measured and rejected: it adds 22 modules, and -- decisively --
// kustomize/api requires google.golang.org/protobuf, so a DEPLOYMENT MANIFEST LINTER would
// join minimal version selection for this service's production protobuf runtime.
//
// That is precisely the argument this repo already used to keep go-task and buf out of the
// tool directive (see Taskfile.yml). Applying it to buf and not to kustomize would be
// inconsistent, so: yaml.v3 (already a dependency, zero new modules) asserts the CONTENT
// here, and `task verify:deploy` shells out to kubectl for the COMPOSITION. The split is
// honest -- what runs everywhere checks what matters most, and the part needing a tool is
// opt-in.
package k8s

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/example/gomicro/internal/platform/config"
)

// deployment models only the fields under assertion. A full k8s type would need
// k8s.io/api -- the dependency this file exists to avoid.
type deployment struct {
	Spec struct {
		Template struct {
			Spec struct {
				TerminationGracePeriodSeconds int `yaml:"terminationGracePeriodSeconds"`
				SecurityContext               struct {
					RunAsNonRoot   bool `yaml:"runAsNonRoot"`
					RunAsUser      int  `yaml:"runAsUser"`
					SeccompProfile struct {
						Type string `yaml:"type"`
					} `yaml:"seccompProfile"`
				} `yaml:"securityContext"`
				Containers []container `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

type container struct {
	Name            string `yaml:"name"`
	SecurityContext struct {
		AllowPrivilegeEscalation *bool `yaml:"allowPrivilegeEscalation"`
		ReadOnlyRootFilesystem   bool  `yaml:"readOnlyRootFilesystem"`
		RunAsNonRoot             bool  `yaml:"runAsNonRoot"`
		Capabilities             struct {
			Drop []string `yaml:"drop"`
		} `yaml:"capabilities"`
	} `yaml:"securityContext"`

	StartupProbe   probe `yaml:"startupProbe"`
	ReadinessProbe probe `yaml:"readinessProbe"`
	LivenessProbe  probe `yaml:"livenessProbe"`

	Resources struct {
		Requests map[string]string `yaml:"requests"`
		Limits   map[string]string `yaml:"limits"`
	} `yaml:"resources"`

	Env []envVar `yaml:"env"`
}

// envVar is one entry of a container's env list. Order matters -- see
// TestEnvVarReferencesAreDeclaredBeforeUse.
type envVar struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type probe struct {
	GRPC *struct {
		Port int `yaml:"port"`
	} `yaml:"grpc"`
	HTTPGet   map[string]any `yaml:"httpGet"`
	TCPSocket map[string]any `yaml:"tcpSocket"`
	Exec      map[string]any `yaml:"exec"`
}

func loadDeployment(t *testing.T) deployment { return load(t, "deployment.yaml") }

func load(t *testing.T, name string) deployment {
	t.Helper()

	b, err := os.ReadFile(filepath.Join("base", name))
	if err != nil {
		t.Fatalf("read base/%s: %v", name, err)
	}

	var d deployment
	if err := yaml.Unmarshal(b, &d); err != nil {
		t.Fatalf("base/%s is not valid YAML: %v", name, err)
	}
	if len(d.Spec.Template.Spec.Containers) == 0 {
		t.Fatalf("base/%s declares no containers, so every assertion here is vacuous", name)
	}
	return d
}

// everyDeployment returns all workloads, so the security and resource guards cover each of
// them rather than only the one that happened to exist when they were written.
//
// This matters more than it looks. Both guards were added in M10, when orderd was the only
// Deployment, and they read a hardcoded filename -- so the worker added in M8b would have
// slipped past every hardening assertion in this file while the suite stayed green.
func everyDeployment(t *testing.T) map[string]deployment {
	t.Helper()

	found := map[string]deployment{}
	entries, err := os.ReadDir("base")
	if err != nil {
		t.Fatalf("read base/: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		b, err := os.ReadFile(filepath.Join("base", e.Name()))
		if err != nil {
			t.Fatalf("read base/%s: %v", e.Name(), err)
		}
		var kind struct {
			Kind string `yaml:"kind"`
		}
		if err := yaml.Unmarshal(b, &kind); err != nil || kind.Kind != "Deployment" {
			continue
		}
		found[e.Name()] = load(t, e.Name())
	}

	if len(found) < 2 {
		t.Fatalf("found %d Deployments in base/, want at least orderd and worker -- a guard "+
			"that silently covers fewer workloads than exist is worse than no guard", len(found))
	}
	return found
}

// TestGracePeriodExceedsTheDrainSequence is the assertion no YAML linter can make, because it
// spans a manifest and a Go source file.
//
// The shutdown sequence is: flip health to NOT_SERVING, wait DrainDelay for Kubernetes to
// remove the pod from Service endpoints, then GracefulStop for up to GracePeriod. If
// terminationGracePeriodSeconds is smaller than that total, the kubelet SIGKILLs the process
// mid-request -- on EVERY deploy, presenting as intermittent 5xx during rollouts that nobody
// can reproduce afterwards.
//
// Both numbers are defaults someone will eventually tune, in files that do not reference each
// other. This is the only thing connecting them.
func TestGracePeriodExceedsTheDrainSequence(t *testing.T) {
	t.Parallel()

	d := loadDeployment(t)
	grace := time.Duration(d.Spec.Template.Spec.TerminationGracePeriodSeconds) * time.Second

	// The defaults the binary actually compiles in.
	cfg, err := config.Parse(map[string]string{})
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	needed := cfg.Shutdown.DrainDelay + cfg.Shutdown.GracePeriod

	if grace <= needed {
		t.Errorf("terminationGracePeriodSeconds is %s but the shutdown sequence needs %s "+
			"(SHUTDOWN_DRAIN_DELAY %s + SHUTDOWN_GRACE_PERIOD %s).\n\n"+
			"The kubelet will SIGKILL the process mid-request on every deploy. The symptom is "+
			"intermittent 5xx during rollouts, and nothing in either file points at the other.",
			grace, needed, cfg.Shutdown.DrainDelay, cfg.Shutdown.GracePeriod)
	}
}

// TestPodSecurityContextIsHardened pins the controls that make a container compromise
// survivable.
func TestPodSecurityContextIsHardened(t *testing.T) {
	t.Parallel()

	for name, d := range everyDeployment(t) {
		t.Run(name, func(t *testing.T) { assertHardened(t, d) })
	}
}

func assertHardened(t *testing.T, d deployment) {
	t.Helper()

	pod := d.Spec.Template.Spec

	if !pod.SecurityContext.RunAsNonRoot {
		t.Error("runAsNonRoot is not set at pod level")
	}
	if pod.SecurityContext.RunAsUser == 0 {
		t.Error("runAsUser is 0 (root). The distroless:nonroot image uses 65532")
	}
	if pod.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
		t.Errorf("seccompProfile.type = %q, want RuntimeDefault -- without it the container "+
			"keeps the full syscall surface", pod.SecurityContext.SeccompProfile.Type)
	}

	for _, c := range pod.Containers {
		sc := c.SecurityContext

		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("%s: allowPrivilegeEscalation is not explicitly false", c.Name)
		}
		if !sc.ReadOnlyRootFilesystem {
			t.Errorf("%s: readOnlyRootFilesystem is false. A writable root filesystem lets an "+
				"attacker with file write drop a payload anywhere", c.Name)
		}
		if !sc.RunAsNonRoot {
			t.Errorf("%s: runAsNonRoot is not set at container level too", c.Name)
		}

		var dropsAll bool
		for _, cap := range sc.Capabilities.Drop {
			if cap == "ALL" {
				dropsAll = true
			}
		}
		if !dropsAll {
			t.Errorf("%s: capabilities.drop does not include ALL (got %v)", c.Name, sc.Capabilities.Drop)
		}
	}
}

// TestAllThreeProbesAreNativeGRPC keeps grpc_health_probe out of the image.
//
// An httpGet or exec probe would work, and each has a cost: exec needs a binary in a
// distroless image that deliberately has none, and httpGet would check the REST edge rather
// than the port that actually serves gRPC traffic -- so a broken RPC surface could report
// healthy. Native grpc: probes (GA since Kubernetes 1.27) need neither.
func TestAllThreeProbesAreNativeGRPC(t *testing.T) {
	t.Parallel()

	d := loadDeployment(t)

	for _, c := range d.Spec.Template.Spec.Containers {
		for name, p := range map[string]probe{
			"startupProbe":   c.StartupProbe,
			"readinessProbe": c.ReadinessProbe,
			"livenessProbe":  c.LivenessProbe,
		} {
			if p.GRPC == nil {
				t.Errorf("%s: %s is not a native grpc: probe.\n\n"+
					"exec needs a binary the distroless image does not have; httpGet would "+
					"check a different port from the one serving RPCs.", c.Name, name)
				continue
			}
			if p.GRPC.Port != 50051 {
				t.Errorf("%s: %s probes port %d, not the RPC port 50051. A probe against any "+
					"other port can report healthy while the thing customers use is broken",
					c.Name, name, p.GRPC.Port)
			}
			if p.HTTPGet != nil || p.TCPSocket != nil || p.Exec != nil {
				t.Errorf("%s: %s declares more than one probe mechanism", c.Name, name)
			}
		}
	}
}

// TestResourcesAreDeclaredWithNoCPULimit covers scheduling and a specific latency trap.
//
// Requests are what the scheduler places on and what protects a pod from being evicted first.
// A CPU LIMIT, though, causes CFS throttling: a Go service that briefly exceeds its quota is
// stalled for the remainder of the 100ms period, which surfaces as p99 latency with no
// corresponding CPU saturation -- one of the hardest things to diagnose in Kubernetes.
//
// Memory keeps its limit because memory is not compressible: exceeding it must OOM-kill
// rather than quietly degrade every other pod on the node.
func TestResourcesAreDeclaredWithNoCPULimit(t *testing.T) {
	t.Parallel()

	for name, d := range everyDeployment(t) {
		t.Run(name, func(t *testing.T) { assertResources(t, d) })
	}
}

func assertResources(t *testing.T, d deployment) {
	t.Helper()

	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Resources.Requests["cpu"] == "" {
			t.Errorf("%s: no cpu request. The scheduler cannot place it sensibly and it is "+
				"evicted first under pressure", c.Name)
		}
		if c.Resources.Requests["memory"] == "" {
			t.Errorf("%s: no memory request", c.Name)
		}
		if c.Resources.Limits["memory"] == "" {
			t.Errorf("%s: no memory limit. Memory is not compressible -- without a limit a leak "+
				"takes the whole node down instead of one pod", c.Name)
		}
		if cpu := c.Resources.Limits["cpu"]; cpu != "" {
			t.Errorf("%s: a cpu limit of %q is set.\n\n"+
				"CFS throttling stalls the container for the rest of each 100ms period once the "+
				"quota is spent, producing p99 latency with no visible CPU saturation. Requests "+
				"schedule; cpu limits mostly just hurt.", c.Name, cpu)
		}
	}
}

// TestAdminPortIsNotInTheService keeps /metrics and /debug/pprof off the cluster network.
//
// Exposing the admin port through a Service makes it reachable by anything that can resolve
// the name, which in a default cluster is everything -- a heap dumper and a CPU-profiler
// trigger for any pod that wanders past. Prometheus scrapes pod IPs directly and needs no
// Service entry.
func TestAdminPortIsNotInTheService(t *testing.T) {
	t.Parallel()

	b, err := os.ReadFile(filepath.Join("base", "service.yaml"))
	if err != nil {
		t.Fatalf("read the service: %v", err)
	}

	var svc struct {
		Spec struct {
			Ports []struct {
				Name string `yaml:"name"`
				Port int    `yaml:"port"`
			} `yaml:"ports"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(b, &svc); err != nil {
		t.Fatalf("the service manifest is not valid YAML: %v", err)
	}
	if len(svc.Spec.Ports) == 0 {
		t.Fatal("the service declares no ports, so this guard is vacuous")
	}

	cfg, err := config.Parse(map[string]string{})
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	_ = cfg // the admin port is a literal below; config only names the default address

	for _, p := range svc.Spec.Ports {
		if p.Port == 9090 || p.Name == "admin" {
			t.Errorf("the Service exposes the admin port (%s/%d).\n\n"+
				"It carries /metrics and /debug/pprof. Anything in the cluster that can resolve "+
				"this Service could then dump the heap.", p.Name, p.Port)
		}
	}
}

// envVarRef matches a $(VAR) reference in an env value.
var envVarRef = regexp.MustCompile(`\$\(([A-Za-z_][A-Za-z0-9_]*)\)`)

// TestEnvVarReferencesAreDeclaredBeforeUse catches a forward reference the kubelet silently
// leaves unexpanded.
//
// # The bug this was written for
//
// Both Deployments read the pod IP into ADMIN_ADDR so the admin surface binds somewhere a
// cluster-local scraper can reach and nothing outside can:
//
//   - name: ADMIN_ADDR
//     value: "$(POD_IP):9090"
//   - name: POD_IP
//     valueFrom: {fieldRef: {fieldPath: status.podIP}}
//
// That is broken, and it shipped. The kubelet expands $(VAR) in a single IN-ORDER pass over
// the container's env list, resolving each entry against only the variables defined BEFORE it;
// an unresolved reference is left in the string verbatim rather than reported. So the container
// receives ADMIN_ADDR="$(POD_IP):9090" literally, net.Listen rejects it, and the admin listener
// never binds -- no /metrics, no pprof, and no error that names the cause.
//
// # Why nothing else caught it
//
// The end-to-end tier runs the full stack and scrapes the worker's /metrics successfully,
// because compose sets ADMIN_ADDR to a literal 0.0.0.0:9090. The only environment that performs
// this expansion is Kubernetes, and no test had ever started one. `kubectl kustomize` builds
// the manifest happily too -- the reference is valid YAML and valid k8s; it is only the ORDER
// that is wrong, and nothing in the toolchain checks order.
//
// So the guard is here, where it costs nothing and reads the shipped file.
func TestEnvVarReferencesAreDeclaredBeforeUse(t *testing.T) {
	t.Parallel()

	for name, d := range everyDeployment(t) {
		for _, c := range d.Spec.Template.Spec.Containers {
			declared := map[string]bool{}

			for _, e := range c.Env {
				for _, m := range envVarRef.FindAllStringSubmatch(e.Value, -1) {
					ref := m[1]
					if declared[ref] {
						continue
					}
					t.Errorf("base/%s: container %q sets %s=%q, but %s is not declared until "+
						"AFTER it in the same env list.\n\n"+
						"The kubelet expands $(VAR) against earlier entries only, and leaves an "+
						"unresolved reference in place verbatim -- so this container starts with "+
						"the literal string %q. Move %s above %s.",
						name, c.Name, e.Name, e.Value, ref, e.Value, ref, e.Name)
				}
				declared[e.Name] = true
			}
		}
	}
}

// envIndexPatch matches a JSON-patch path that addresses an env entry by POSITION.
var envIndexPatch = regexp.MustCompile(`/env/\d+`)

// TestNoOverlayPatchesEnvByIndex forbids the reference that silently retargets.
//
// # The bug
//
// The dev overlay patched `/spec/template/spec/containers/0/env/5/value` intending AUTH_MODE.
// env/5 is STORE_DRIVER. So the overlay set STORE_DRIVER=dev -- which config.Validate refuses,
// the only valid drivers being memory and postgres -- and left AUTH_MODE=oidc untouched. Every
// orderd pod in the dev overlay failed validation and CrashLoopBackOffed before opening a
// listener, while the comment beside the patch described the opposite.
//
// # Why nothing caught it
//
// `kubectl kustomize` renders the wrong patch perfectly happily: the index is in range and the
// output is valid YAML, so `task verify:deploy` passes. Nothing type-checks a positional
// reference, and nothing in this repo had ever applied the overlay to a cluster. Adding or
// reordering ONE env entry in the base silently repoints every index below it -- and this repo
// reorders env entries for real reasons (POD_IP must precede ADMIN_ADDR; see deployment.yaml).
//
// A strategic-merge patch addressing entries by NAME cannot retarget, so this bans the form
// rather than checking the arithmetic.
func TestNoOverlayPatchesEnvByIndex(t *testing.T) {
	t.Parallel()

	var checked int

	err := filepath.WalkDir("overlays", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".yaml" {
			return err
		}
		checked++

		b, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if !envIndexPatch.MatchString(line) {
				continue
			}
			t.Errorf("%s addresses an env entry by index:\n\n  %s\n\n"+
				"A positional reference retargets silently whenever the base's env list is "+
				"reordered or an entry is inserted above it, and kustomize renders the wrong "+
				"result without complaint. Patch by name with a strategic merge patch instead.",
				filepath.ToSlash(path), strings.TrimSpace(line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk overlays/: %v", err)
	}
	if checked == 0 {
		t.Fatal("found no overlay YAML; this guard would pass forever")
	}
}
