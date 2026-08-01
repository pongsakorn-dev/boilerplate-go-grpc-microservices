package test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/testutil"
)

// forbiddenInDomain lists what the domain layer must never reach, and why.
//
// The "why" is not decoration. When this test fails, the person who broke it is usually
// mid-feature and needs to understand in one screen whether to move their code or change
// the rule.
var forbiddenInDomain = []struct {
	path  string
	what  string
	exact bool // match this import exactly, rather than as a prefix
}{
	{path: "github.com/example/gomicro/gen/", what: "generated protobuf code"},
	{path: "google.golang.org/grpc", what: "gRPC"},
	{path: "google.golang.org/protobuf", what: "the protobuf runtime"},
	{path: "github.com/grpc-ecosystem/", what: "grpc-gateway"},
	{path: "gorm.io/", what: "GORM"},
	{path: "go.opentelemetry.io/", what: "OpenTelemetry"},
	{path: "github.com/prometheus/", what: "Prometheus"},
	{path: "github.com/redis/", what: "Redis"},
	{path: "github.com/nats-io/", what: "NATS"},
	{path: "github.com/testcontainers/", what: "testcontainers"},

	// Exact, not prefix.
	//
	// database/sql is the real thing -- a domain package opening connections is the
	// violation. database/sql/driver is NOT: it is a tiny interface-only package, and
	// github.com/google/uuid imports it solely to implement Valuer and Scanner so a UUID
	// can be used as a column value. Banning it by prefix would fail this test for a
	// dependency that touches no database at all.
	{path: "database/sql", what: "database/sql", exact: true},

	// Exact for the same reason: net/http is a real transport, but net/http's own
	// internal tree and net/url are ordinary utilities.
	{path: "net/http", what: "net/http", exact: true},
}

// TestDomainImportsNothingHeavy turns the layering rule into a compile-time property.
//
// "The domain must not depend on infrastructure" is the single most-stated and
// least-enforced rule in Go service architecture. A comment saying it is a wish; a diagram
// saying it is decoration. This walks the real transitive import graph and fails the build.
//
// The payoff is concrete rather than aesthetic: it is what guarantees the business tests
// run in milliseconds with no Docker daemon, and what keeps a proto field rename from
// becoming a database migration.
func TestDomainImportsNothingHeavy(t *testing.T) {
	t.Parallel()

	// Both the domain package and the in-memory store, because the latter is a
	// production code path (STORE_DRIVER=memory), not a test double.
	for _, pkg := range []string{
		"./internal/order",
		"./internal/order/ordermem",
	} {
		t.Run(pkg, func(t *testing.T) {
			t.Parallel()

			deps := testutil.GoList(t, "-deps", "-f", "{{.ImportPath}}", pkg)

			var violations []string
			for _, dep := range deps {
				for _, f := range forbiddenInDomain {
					match := dep == f.path
					if !f.exact {
						match = match || strings.HasPrefix(dep, f.path)
					}
					if match {
						violations = append(violations, "  "+dep+"  ("+f.what+")")
					}
				}
			}

			if len(violations) > 0 {
				sort.Strings(violations)
				t.Errorf("%s transitively imports infrastructure:\n%s\n\n"+
					"The domain layer must depend on nothing but the standard library and small\n"+
					"leaf utilities. Move the offending code to internal/grpcapi (if it is about\n"+
					"the wire format) or internal/platform (if it is cross-cutting), and have the\n"+
					"domain define an interface the adapter implements.",
					pkg, strings.Join(violations, "\n"))
			}
		})
	}
}

// TestPlatformDoesNotImportServices keeps the cross-cutting layer service-agnostic.
//
// internal/platform is the code a second service reuses verbatim, and the code that would
// be extracted into a shared module the day this repo grows past one team. The moment a
// platform package imports internal/order, that extraction stops being possible and the
// "platform" becomes a pile of order-service helpers with a misleading name.
func TestPlatformDoesNotImportServices(t *testing.T) {
	t.Parallel()

	const (
		platformPrefix = "github.com/example/gomicro/internal/platform"
		orderPrefix    = "github.com/example/gomicro/internal/order"
		grpcapiPrefix  = "github.com/example/gomicro/internal/grpcapi"
	)

	pkgs := testutil.GoList(t, "-f", "{{.ImportPath}}", "./internal/platform/...")
	if len(pkgs) == 0 {
		t.Fatal("found no platform packages -- this guard would silently pass forever")
	}

	for _, pkg := range pkgs {
		deps := testutil.GoList(t, "-deps", "-f", "{{.ImportPath}}", pkg)

		for _, dep := range deps {
			if strings.HasPrefix(dep, orderPrefix) || strings.HasPrefix(dep, grpcapiPrefix) {
				t.Errorf("%s imports %s\n\n"+
					"Packages under internal/platform must not know about a specific service.\n"+
					"Invert it: define the type or interface in platform and let the service\n"+
					"depend on it, not the other way round.", pkg, dep)
			}
		}
		_ = platformPrefix
	}
}

// TestOnlyConfigReadsTheEnvironment keeps configuration in exactly one place.
//
// Configuration scattered across packages is how a service ends up with behaviour that
// cannot be reproduced from its deployment manifest: some setting is read directly from
// os.Getenv three layers down, documented nowhere, and validated never.
func TestOnlyConfigReadsTheEnvironment(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)
	allowed := map[string]bool{
		// The one legitimate reader.
		"internal/platform/config/config.go": true,
		// devtool is a developer CLI, not the service.
		"cmd/devtool/main.go": true,
	}

	var offenders []string
	for _, rel := range testutil.TrackedFiles(t) {
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		if strings.HasPrefix(rel, "gen/") || allowed[rel] {
			continue
		}
		if containsEnvRead(t, root, rel) {
			offenders = append(offenders, rel)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("these files read the environment directly:\n  %s\n\n"+
			"Add the setting to internal/platform/config instead, where it is documented,\n"+
			"validated before any listener binds, and visible in one place.",
			strings.Join(offenders, "\n  "))
	}
}

// containsEnvRead reports whether a source file reaches for the process environment.
func containsEnvRead(t *testing.T, root, rel string) bool {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return false
	}
	src := string(b)
	for _, call := range []string{"os.Getenv(", "os.LookupEnv(", "os.Environ("} {
		if strings.Contains(src, call) {
			return true
		}
	}
	return false
}
