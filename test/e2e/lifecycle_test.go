//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
	"github.com/example/gomicro/internal/platform/migrations"
)

const (
	gatewayURL = "http://localhost:8080"
	grpcAddr   = "localhost:50051"
)

func inUse(port string) bool {
	conn, err := net.DialTimeout("tcp", "localhost:"+port, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// dialGRPC connects to the containerised server.
func dialGRPC(t *testing.T) orderv1.OrderServiceClient {
	t.Helper()

	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", grpcAddr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return orderv1.NewOrderServiceClient(conn)
}

// createOrder posts one order through the REST edge and returns its id.
//
// Through REST rather than gRPC on purpose: it exercises the transcoder, the JSON contract and
// the gRPC chain in a single call, and it is the surface a reader is most likely to try first.
func createOrder(t *testing.T, sku string) string {
	t.Helper()

	body := fmt.Sprintf(`{"customer_id":"e2e-customer","items":[
		{"sku":%q,"quantity":1,"unit_price":{"currency_code":"USD","units":"7","nanos":500000000}}]}`, sku)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/v1/orders", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/orders: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var decoded struct {
		Order struct {
			OrderID  string `json:"order_id"`
			TenantID string `json:"tenant_id"`
			Status   string `json:"status"`
		} `json:"order"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/orders returned %d", resp.StatusCode)
	}
	if decoded.Order.OrderID == "" {
		t.Fatal("the response carries no order id")
	}
	return decoded.Order.OrderID
}

// TestTheStackServesTrafficOverBothSurfaces is the baseline: the real images, wired by the real
// compose file, answer on both ports.
func TestTheStackServesTrafficOverBothSurfaces(t *testing.T) {
	id := createOrder(t, "E2E-SKU-1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// And the same order comes back over gRPC -- the same store, reached by the other
	// transport, which is the claim "REST is transcoded onto the same service" made concrete.
	got, err := dialGRPC(t).GetOrder(ctx, &orderv1.GetOrderRequest{OrderId: id})
	if err != nil {
		t.Fatalf("GetOrder over gRPC: %v", err)
	}
	if got.GetOrder().GetOrderId() != id {
		t.Errorf("gRPC returned order %q, want %q", got.GetOrder().GetOrderId(), id)
	}
	if got.GetOrder().GetTenantId() == "" {
		t.Error("the order has no tenant; the auth interceptor did not populate a principal")
	}
}

// TestThePersistedStoreIsReallyPostgres guards against the stack silently falling back to the
// in-memory driver.
//
// STORE_DRIVER is set in the compose file, and if it were dropped every other test here would
// still pass -- the in-memory store satisfies the same contract. The difference only shows up
// as data that vanishes on restart, which is exactly the failure a deployment test should catch.
func TestThePersistedStoreIsReallyPostgres(t *testing.T) {
	id := createOrder(t, "E2E-SKU-PERSIST")

	out := psql(t, fmt.Sprintf("SELECT count(*) FROM orders WHERE id = '%s'", id))
	if strings.TrimSpace(out) != "1" {
		t.Errorf("the order is not in Postgres (count = %q).\n\n"+
			"The service is answering from the in-memory store, so everything it has been told "+
			"disappears on the next restart.", strings.TrimSpace(out))
	}
}

// psql runs one query inside the postgres container and returns the bare result.
func psql(t *testing.T, query string) string {
	t.Helper()

	// -tA: tuples only, unaligned. Anything else has to be parsed out of a formatted table.
	return composeExec(t, "postgres", "psql", "-U", "gomicro", "-d", "gomicro", "-tAc", query)
}

// TestMigrationsRanBeforeTheServerAccepted covers the ordering compose is asked to enforce.
//
// orderd depends on migrate with service_completed_successfully, which is the compose analogue
// of the Kubernetes Job that must finish before the Deployment rolls -- and the reason
// migrations never run on server boot. If that dependency were dropped, orderd would start
// against an empty schema and fail its first query, intermittently, depending on which
// container won.
func TestMigrationsRanBeforeTheServerAccepted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	out, err := compose(ctx, "ps", "-a", "--format", "{{.Service}} {{.State}} {{.ExitCode}}")
	if err != nil {
		t.Fatalf("compose ps: %v\n%s", err, out)
	}

	var sawMigrate bool
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 || fields[0] != "migrate" {
			continue
		}
		sawMigrate = true
		if fields[1] != "exited" || fields[2] != "0" {
			t.Errorf("the migrate service is %s with exit code %s, want exited/0", fields[1], fields[2])
		}
	}
	if !sawMigrate {
		t.Fatal("no migrate container exists, so nothing applied the schema")
	}

	// And the schema is actually current, rather than merely having been attempted.
	//
	// THE EXPECTED VERSION IS DERIVED, not written down. It used to be the literal "2", which
	// meant every migration added to the repo broke this test for a reason that had nothing to
	// do with what it asserts -- and broke it only in the slowest tier, so it was found long
	// after the change that caused it. (It was: 00003 and 00004 both landed before anyone ran
	// this.) A guard that fails on unrelated work is a guard people learn to edit past.
	//
	// Reading the highest embedded migration keeps the real assertion -- the container ran
	// them ALL, not just some -- while making an added migration a non-event here.
	want := highestMigrationVersion(t)
	if got := strings.TrimSpace(psql(t, "SELECT max(version_id) FROM goose_db_version")); got != want {
		t.Errorf("goose reports schema version %q, want %q.\n\n"+
			"The migrate container exited zero without applying every migration, so the "+
			"service is running against a schema older than the code expects.", got, want)
	}
}

// highestMigrationVersion reads the newest version out of the embedded migration set.
//
// The filenames are goose's own contract -- NNNNN_name.sql -- so the leading number is the
// version. Parsed rather than counted: a repo that ever skips or renumbers a file would make a
// count wrong while leaving the maximum right.
func highestMigrationVersion(t *testing.T) string {
	t.Helper()

	entries, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("glob the embedded migrations: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no embedded migrations found; this assertion would be meaningless")
	}

	var highest int64
	for _, name := range entries {
		digits, _, ok := strings.Cut(filepath.Base(name), "_")
		if !ok {
			t.Fatalf("migration %q is not named NNNNN_name.sql", name)
		}
		v, err := strconv.ParseInt(digits, 10, 64)
		if err != nil {
			t.Fatalf("migration %q has a non-numeric version prefix: %v", name, err)
		}
		if v > highest {
			highest = v
		}
	}
	return strconv.FormatInt(highest, 10)
}

// TestTheImageHasNoShell keeps the attack surface where the Dockerfile claims it is.
//
// distroless/static:nonroot ships no shell, which means a remote-code-execution bug in the
// service cannot be turned into a shell by the usual means. It is one `FROM` line away from
// being untrue, and nothing else in the repository would notice.
func TestTheImageHasNoShell(t *testing.T) {
	for _, shell := range []string{"/bin/sh", "/bin/bash", "/busybox/sh"} {
		t.Run(shell, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			cmd := exec.CommandContext(ctx, "docker", "run", "--rm",
				"--entrypoint", shell, imageRef(t, "orderd"), "-c", "echo reachable")
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Errorf("%s ran inside the image and printed %q.\n\n"+
					"The base image is supposed to contain no shell at all.", shell, strings.TrimSpace(string(out)))
			}
		})
	}
}

// imageRef returns the image compose actually built for a service.
//
// Read from compose rather than hardcoded: the tag is derived from the project name, so writing
// it out here would produce a test that passes against a stale image left by an earlier run.
func imageRef(t *testing.T, service string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// `compose ps`, not `compose images`: the latter's --format rejects a Go template in this
	// Compose version and falls back to printing a table, which is a parser waiting to break.
	out, err := compose(ctx, "ps", "--format", "{{.Service}} {{.Image}}")
	if err != nil {
		t.Fatalf("compose ps: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && fields[0] == service {
			return fields[1]
		}
	}
	t.Fatalf("compose knows no image for service %q:\n%s", service, out)
	return ""
}

// TestTheHealthCommandDiscriminates is the test that catches a healthcheck which is not one.
//
// A container healthcheck has two ways to be wrong and they fail in opposite directions:
//
//	never passes    loud. `compose up --wait` hangs and then errors, and everyone notices.
//	always passes   SILENT. The orchestrator believes a broken service is healthy, routes to
//	                it, and restarts nothing.
//
// This repository has now shipped both. M10's `/orderd --help` was the first kind: the binary
// did not parse flags, so instead of printing usage it started a second server inside the
// container and raced the first for the port. Adding flag parsing to support `-health` turned
// it into the second kind -- Go's flag package answers `--help` by printing usage and exiting
// ZERO, so the identical healthcheck line began passing unconditionally, on a dead service as
// readily as a live one.
//
// Every other test here would still pass with that healthcheck, because they all talk to the
// service directly. The only way to catch it is to check that the command DISTINGUISHES: it
// must succeed against a serving process and fail against nothing.
func TestTheHealthCommandDiscriminates(t *testing.T) {
	// It succeeds where a server is running -- the running container, probing itself exactly
	// as the compose healthcheck does.
	if out := composeExec(t, "orderd", "/orderd", "-health"); !strings.Contains(out, "SERVING") {
		t.Errorf("the health command did not report SERVING inside a healthy container: %q", out)
	}

	// And the command THE CONTAINER IS ACTUALLY CONFIGURED WITH fails where none is.
	//
	// Read from `docker inspect` rather than written out here, and that distinction is the
	// whole test. An earlier version hardcoded `-health`: it proved that `-health`
	// discriminates and said nothing about whether compose uses it, so reverting the
	// healthcheck line to `--help` left this passing. Whatever the healthcheck is, it has to
	// be capable of failing.
	configured := configuredHealthcheck(t)
	if len(configured) == 0 {
		t.Fatal("the orderd container declares no healthcheck at all")
	}
	t.Logf("the container's configured healthcheck is: %v", configured)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	args := append([]string{"run", "--rm", "--entrypoint", configured[0], imageRef(t, "orderd")}, configured[1:]...)
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()

	if err == nil {
		t.Errorf("the configured healthcheck %v succeeded with no server running, printing %q.\n\n"+
			"A healthcheck that cannot fail is worse than none: the orchestrator believes a "+
			"dead service is healthy, keeps routing traffic to it, and never restarts it. "+
			"`--help` behaves exactly like this -- Go's flag package prints usage and exits zero.",
			configured, strings.TrimSpace(string(out)))
	}
}

// configuredHealthcheck returns the command the running container probes itself with.
//
// The leading "CMD" that compose requires is stripped. "CMD-SHELL" is rejected outright: this
// image has no shell, so such a healthcheck could only ever fail.
func configuredHealthcheck(t *testing.T) []string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	name := containerName(t, "orderd")
	out, err := exec.CommandContext(ctx, "docker", "inspect", name,
		"--format", "{{json .Config.Healthcheck.Test}}").CombinedOutput()
	if err != nil {
		t.Fatalf("inspect %s: %v\n%s", name, err, out)
	}

	var test []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &test); err != nil {
		t.Fatalf("decode the healthcheck %q: %v", strings.TrimSpace(string(out)), err)
	}
	if len(test) == 0 {
		return nil
	}

	switch test[0] {
	case "CMD":
		return test[1:]
	case "CMD-SHELL":
		t.Fatalf("the healthcheck is CMD-SHELL %q, but this image has no shell, so it could "+
			"only ever fail", strings.Join(test[1:], " "))
	}
	return test
}

// containerName resolves a compose service to its container.
func containerName(t *testing.T, service string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	out, err := compose(ctx, "ps", "--format", "{{.Service}} {{.Name}}")
	if err != nil {
		t.Fatalf("compose ps: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && fields[0] == service {
			return fields[1]
		}
	}
	t.Fatalf("no container for service %q:\n%s", service, out)
	return ""
}
