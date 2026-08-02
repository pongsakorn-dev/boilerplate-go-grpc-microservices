//go:build integration

// Package testdb runs the integration tier against a real PostgreSQL.
//
// BUILD-TAGGED, not testing.Short(). The distinction is the whole reason `go test ./...` is
// safe with the Docker daemon stopped: a Short() skip still LINKS testcontainers into every
// test binary in the module, so the dependency is present, compiled, and its init functions
// run. A build tag makes the default tier's dependency set a compile-time property, which
// test/tiers_test.go then asserts.
//
// Run it with:
//
//	go test -tags=integration ./...
package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver, for the admin connection
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/example/gomicro/internal/platform/migrations"
)

// postgresImage is pinned. "latest" makes a test suite that passes today and fails on an
// unrelated morning, for a reason nobody changed.
const postgresImage = "postgres:17-alpine"

// templateDB is migrated once and then cloned per test. See Fresh.
const templateDB = "gomicro_template"

// Instance is one container, shared by every test in a package.
type Instance struct {
	container *tcpostgres.PostgresContainer
	adminDSN  string
	counter   atomic.Int64
}

// RequireDocker skips -- never fails -- when Docker is unreachable.
//
// SKIPPING IS THE POINT. A developer without Docker running should see "skipped, here is
// why", not a wall of red that looks like their change broke something. A failing
// integration suite on a laptop trains people to ignore failures, which is how a real one
// gets missed.
//
// The message names the ACTIVE DOCKER CONTEXT because that is the actual cause on Windows
// more often than the daemon being down: testcontainers resolves the default docker_engine
// pipe, while Docker Desktop publishes desktop-linux, so a running daemon is invisible to it.
// Printing the context turns a twenty-minute confusion into a one-line fix.
func RequireDocker(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	provider, err := testcontainers.NewDockerProvider()
	if err == nil {
		err = provider.Health(ctx)
		_ = provider.Close()
	}
	if err == nil {
		return
	}

	t.Skipf("skipping: Docker is not reachable (%v)\n"+
		"  active docker context: %s\n"+
		"  This tier needs a Docker daemon. Start Docker Desktop, or run only the default\n"+
		"  tier with `go test ./...` (no -tags=integration), which needs nothing at all.",
		err, activeDockerContext())
}

// activeDockerContext reports the context Docker is configured to use, best-effort.
func activeDockerContext() string {
	out, err := exec.Command("docker", "context", "show").Output()
	if err != nil {
		return "unknown (docker CLI not on PATH)"
	}
	return strings.TrimSpace(string(out))
}

// Start boots one container and migrates a TEMPLATE database.
//
// Call it from TestMain so the cost -- a few seconds of image start plus one migration run --
// is paid once per package rather than once per test.
func Start(ctx context.Context) (*Instance, error) {
	container, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("gomicro"),
		tcpostgres.WithPassword("gomicro"),
		testcontainers.WithWaitStrategy(
			// Postgres in its entrypoint starts, stops, and restarts the server, so the
			// first "ready to accept connections" is a LIE -- connecting on it produces
			// intermittent "the database system is starting up" failures that look like
			// flakes. Occurrence(2) waits for the real one.
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres container: %w", err)
	}

	adminDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, fmt.Errorf("container connection string: %w", err)
	}

	inst := &Instance{container: container, adminDSN: adminDSN}
	if err := inst.buildTemplate(ctx); err != nil {
		_ = container.Terminate(ctx)
		return nil, err
	}
	return inst, nil
}

// buildTemplate creates the template database and migrates it once.
func (i *Instance) buildTemplate(ctx context.Context) error {
	admin, err := sql.Open("pgx", i.adminDSN)
	if err != nil {
		return fmt.Errorf("open admin connection: %w", err)
	}
	defer func() { _ = admin.Close() }()

	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+templateDB); err != nil {
		return fmt.Errorf("create template database: %w", err)
	}

	tmplDB, err := sql.Open("pgx", replaceDBName(i.adminDSN, templateDB))
	if err != nil {
		return fmt.Errorf("open template connection: %w", err)
	}
	if err := migrations.Up(ctx, tmplDB); err != nil {
		_ = tmplDB.Close()
		return fmt.Errorf("migrate template: %w", err)
	}

	// CLOSING MATTERS. Postgres refuses CREATE DATABASE ... TEMPLATE while any session is
	// connected to the template, with "source database is being accessed by other users".
	// Leaving this open makes the FIRST test pass (it built the template) and every
	// subsequent one fail -- a failure that reads like a race and is not.
	if err := tmplDB.Close(); err != nil {
		return fmt.Errorf("close template connection: %w", err)
	}
	return nil
}

// Fresh returns a DSN for a brand-new, fully-migrated, empty database.
//
// CREATE DATABASE ... TEMPLATE copies the already-migrated template at the filesystem level,
// which takes tens of milliseconds. The alternatives are worse: re-running migrations per
// test costs seconds each, and TRUNCATE between tests shares one database, so tests cannot
// run in parallel and any leaked row from one is a mystery in the next.
func (i *Instance) Fresh(t *testing.T) string {
	t.Helper()

	ctx := context.Background()
	name := fmt.Sprintf("gomicro_t%d", i.counter.Add(1))

	admin, err := sql.Open("pgx", i.adminDSN)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer func() { _ = admin.Close() }()

	if _, err := admin.ExecContext(ctx,
		fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, templateDB)); err != nil {
		t.Fatalf("clone template database: %v", err)
	}

	// Deliberately NOT dropped on cleanup. The container is torn down at the end of the
	// package, taking every database with it, and DROP DATABASE serialises against open
	// connections -- so dropping here would add flakiness and time to buy nothing.
	return replaceDBName(i.adminDSN, name)
}

// Terminate stops the container.
func (i *Instance) Terminate(ctx context.Context) error {
	if i == nil || i.container == nil {
		return nil
	}
	return i.container.Terminate(ctx)
}

// replaceDBName swaps the database name in a postgres URL.
//
// String surgery rather than net/url parsing because the container's DSN is a well-formed
// URL whose path is exactly "/dbname"; parsing and re-encoding it risks re-escaping the
// password, which testcontainers already encoded.
func replaceDBName(dsn, name string) string {
	idx := strings.LastIndex(dsn, "/")
	if idx < 0 {
		return dsn
	}
	rest := ""
	if q := strings.Index(dsn[idx:], "?"); q >= 0 {
		rest = dsn[idx+q:]
	}
	return dsn[:idx+1] + name + rest
}
