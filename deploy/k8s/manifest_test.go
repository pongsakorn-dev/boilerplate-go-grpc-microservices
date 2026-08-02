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
}

type probe struct {
	GRPC *struct {
		Port int `yaml:"port"`
	} `yaml:"grpc"`
	HTTPGet   map[string]any `yaml:"httpGet"`
	TCPSocket map[string]any `yaml:"tcpSocket"`
	Exec      map[string]any `yaml:"exec"`
}

func loadDeployment(t *testing.T) deployment {
	t.Helper()

	b, err := os.ReadFile(filepath.Join("base", "deployment.yaml"))
	if err != nil {
		t.Fatalf("read the base deployment: %v", err)
	}

	var d deployment
	if err := yaml.Unmarshal(b, &d); err != nil {
		t.Fatalf("the deployment manifest is not valid YAML: %v", err)
	}
	if len(d.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("the deployment declares no containers, so every assertion here is vacuous")
	}
	return d
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

	d := loadDeployment(t)
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

	d := loadDeployment(t)

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
