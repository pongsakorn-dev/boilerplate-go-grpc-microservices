//go:build e2e

// Package oidc runs the shipped stack with AUTH_MODE=oidc against a real Keycloak.
//
// WHY THIS IS ITS OWN PACKAGE AND ITS OWN STACK. The sibling e2e package runs AUTH_MODE=dev,
// where every request arrives as a fixed full-scope principal. That is the right default for
// the rest of the suite -- it keeps those tests about delivery, persistence and shutdown
// rather than about credentials -- but it means none of them exercise the code path that
// actually verifies anything. One stack cannot be both.
//
// WHY IT IS WORTH A SECOND STACK, which is the objection this milestone was deferred on twice.
// AUTH_MODE=oidc is the highest-consequence configuration in the repository, and until now it
// was verified by hand exactly once. This service has already shipped a build where the server
// never called the verifier: AUTH_MODE=oidc parsed, validated, booted with no warning, and
// served every request as a full-scope dev principal. Every unit test passed. What found it
// was someone calling an RPC with no credential and getting three orders back -- which is,
// almost exactly, TestAnAnonymousCallIsRefused below.
//
// The cost is about ninety seconds of wall clock and one overlay file. The thing it covers is
// the difference between an authenticated service and one that only looks authenticated.
//
// Every token here is fetched from INSIDE the compose network, via `wget` in the nats
// container. Keycloak stamps `iss` from the host it was asked on, so a token obtained from the
// host through the published port would carry an issuer orderd does not accept -- and the test
// would fail for a reason that has nothing to do with the service.
package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
)

const (
	project  = "gomicro-e2e-oidc"
	baseFile = "../../../deploy/compose/docker-compose.yml"
	oidcFile = "../../../deploy/compose/docker-compose.oidc.yml"

	grpcAddr = "localhost:50051"

	// realmURL as seen from INSIDE the compose network. This is also what orderd is
	// configured with, which is what makes the issuer check pass.
	realmURL = "http://keycloak:8080/realms/gomicro"
)

// unavailable explains why the stack could not be started, or is nil.
//
// See the sibling e2e package for the full reasoning: an early os.Exit(0) makes `go test` print
// "ok" for a tier that ran nothing, which reads exactly like a pass -- and here it would hide
// the only automated check that AUTH_MODE=oidc verifies anything at all.
var unavailable error

// requireStack skips the calling test when the OIDC stack is not running. Every test in this
// package must call it first; test/e2e/skips_test.go enforces that for both packages.
func requireStack(t *testing.T) {
	t.Helper()

	if unavailable != nil {
		t.Skipf("the OIDC compose stack is not available: %v", unavailable)
	}
}

func TestMain(m *testing.M) {
	if err := requireDocker(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/oidc: %v\n", err)

		// Run the tests anyway, so each one reports SKIP instead of the package reporting ok.
		unavailable = err
		os.Exit(m.Run())
	}

	down()

	if err := requireFreePorts(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/oidc: %v\n", err)
		os.Exit(1)
	}

	code := 1
	func() {
		defer down()

		if err := up(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e/oidc: bringing the stack up failed: %v\n", err)
			dumpLogs()
			return
		}
		code = m.Run()
		if code != 0 {
			dumpLogs()
		}
	}()

	os.Exit(code)
}

// TestAnAnonymousCallIsRefused is the assertion that would have caught the M5 bypass.
//
// It is deliberately the least clever test in this package: call an ordinary RPC with no
// credential at all and require Unauthenticated. A service that answers this is not
// authenticating anybody, whatever AUTH_MODE says and whatever its unit tests prove about the
// verifier in isolation.
func TestAnAnonymousCallIsRefused(t *testing.T) {
	requireStack(t)

	client := orderv1.NewOrderServiceClient(dial(t))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := client.ListOrders(ctx, &orderv1.ListOrdersRequest{})

	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("an unauthenticated ListOrders returned %v, want Unauthenticated (err=%v).\n\n"+
			"With AUTH_MODE=oidc this service must refuse a request carrying no credential. "+
			"If this returned OK, the verifier is not in the request path -- which is exactly "+
			"the shape of the bypass this repository shipped in M5: a correct verifier that "+
			"the server never called.", got, err)
	}
}

// TestARealServiceTokenIsAccepted is the positive case, and it exercises the whole chain:
// Keycloak signs, orderd fetches JWKS from the issuer, validates signature, issuer, audience
// and expiry, maps claims to a principal, and the policy grants orders:write.
func TestARealServiceTokenIsAccepted(t *testing.T) {
	requireStack(t)

	token := serviceToken(t)
	client := orderv1.NewOrderServiceClient(dial(t))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)

	resp, err := client.CreateOrder(ctx, &orderv1.CreateOrderRequest{
		CustomerId: "oidc-e2e",
		Items: []*orderv1.OrderItem{{
			Sku:       "SKU-OIDC",
			Quantity:  1,
			UnitPrice: &orderv1.Money{CurrencyCode: "USD", Units: 5},
		}},
	})
	if err != nil {
		t.Fatalf("a real token signed by the realm was rejected: %v\n\n"+
			"Signature, issuer, audience and expiry all have to line up. Check that "+
			"OIDC_ISSUER_URL matches the `iss` the realm stamps, and that the "+
			"audience-orderd mapper is still on the orders-worker client.", err)
	}

	// The tenant came from the token's tenant_id claim, not from anything the caller sent.
	if got := resp.GetOrder().GetTenantId(); got == "" {
		t.Error("the created order has no tenant; the tenant_id claim did not reach the principal")
	}
}

// TestScopesAreEnforcedAcrossTheRealBoundary is the authorization half, and it is why this
// package uses two different clients rather than one.
//
// orders-web is granted ONLY orders:read in the realm. A token from it must be able to list
// and must not be able to create. That is a stronger statement than "a valid token works":
// it proves the policy is read from the token's scopes rather than from the fact that a token
// was present at all -- authenticated-therefore-authorized being the usual way this breaks.
func TestScopesAreEnforcedAcrossTheRealBoundary(t *testing.T) {
	requireStack(t)

	token := userToken(t, "alice", "alice")
	client := orderv1.NewOrderServiceClient(dial(t))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)

	// orders:read -- allowed.
	if _, err := client.ListOrders(ctx, &orderv1.ListOrdersRequest{}); err != nil {
		t.Fatalf("a user token with orders:read could not list orders: %v", err)
	}

	// orders:write -- NOT granted to orders-web.
	_, err := client.CreateOrder(ctx, &orderv1.CreateOrderRequest{
		CustomerId: "oidc-e2e-denied",
		Items: []*orderv1.OrderItem{{
			Sku:       "SKU-DENIED",
			Quantity:  1,
			UnitPrice: &orderv1.Money{CurrencyCode: "USD", Units: 5},
		}},
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("a token holding only orders:read created an order and got %v (err=%v).\n\n"+
			"Authentication is being treated as authorization: the request was allowed "+
			"because a valid token was present, not because it carried the scope the method "+
			"requires.", got, err)
	}
}

// TestAGarbageTokenIsRefused covers the other direction: something that LOOKS like a
// credential must not be treated as one.
func TestAGarbageTokenIsRefused(t *testing.T) {
	requireStack(t)

	client := orderv1.NewOrderServiceClient(dial(t))

	for _, tc := range []struct{ name, token string }{
		{"not a jwt at all", "hunter2"},
		{"structurally a jwt, signed by nobody", "eyJhbGciOiJub25lIn0.eyJzdWIiOiJhdHRhY2tlciJ9."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tc.token)

			_, err := client.ListOrders(ctx, &orderv1.ListOrdersRequest{})
			if got := status.Code(err); got != codes.Unauthenticated {
				t.Errorf("%s was answered with %v, want Unauthenticated", tc.name, got)
			}
		})
	}
}

// TestHealthStaysPublicUnderOIDC is the operational counterpart, and it is not a formality.
//
// A kubelet holds no credential and cannot be given one. If turning on OIDC also gated
// grpc.health.v1, every pod would fail its readiness probe and never join the Service --
// a self-inflicted total outage that looks like a crashloop, appearing only in the
// environment where authentication is switched on.
//
// The compose healthcheck (`orderd -health`) exercises the same path, so the stack reaching
// "healthy" at all is itself part of this assertion.
func TestHealthStaysPublicUnderOIDC(t *testing.T) {
	requireStack(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := healthpb.NewHealthClient(dial(t)).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("the health endpoint requires a credential under AUTH_MODE=oidc: %v\n\n"+
			"Kubernetes' native grpc: probe sends no metadata, so every pod would fail "+
			"readiness and never receive traffic.", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("health reports %v, want SERVING", resp.GetStatus())
	}
}

// --- token acquisition ---

// serviceToken gets a client-credentials token for orders-worker, which the realm grants
// orders:read and orders:write.
func serviceToken(t *testing.T) string {
	t.Helper()

	return tokenFrom(t, strings.Join([]string{
		"grant_type=client_credentials",
		"client_id=orders-worker",
		// Committed on purpose: the realm file is a development fixture and says so. A real
		// deployment supplies this from a secret.
		"client_secret=dev-only-not-a-real-secret",
	}, "&"))
}

// userToken gets a password-grant token for a realm user through orders-web, which is granted
// orders:read only.
func userToken(t *testing.T, username, password string) string {
	t.Helper()

	return tokenFrom(t, strings.Join([]string{
		"grant_type=password",
		"client_id=orders-web",
		"username=" + username,
		"password=" + password,
	}, "&"))
}

// tokenFrom posts to the realm's token endpoint from INSIDE the compose network.
//
// Through the nats container because it is busybox and therefore has wget; the Keycloak image
// has neither curl nor wget, and orderd's is distroless with no shell at all. Reaching
// Keycloak from the host instead would produce a token whose issuer orderd rejects -- see the
// package comment.
func tokenFrom(t *testing.T, form string) string {
	t.Helper()

	out := composeExec(t, "nats", "wget", "-q", "-O", "-",
		"--post-data", form,
		realmURL+"/protocol/openid-connect/token")

	var body struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("the token endpoint did not return JSON: %v\n%s", err, out)
	}
	if body.Error != "" {
		t.Fatalf("the realm refused to issue a token: %s (%s)", body.Error, body.Description)
	}
	if body.AccessToken == "" {
		t.Fatalf("the token response carried no access_token:\n%s", out)
	}
	return body.AccessToken
}

// --- stack lifecycle ---

func dial(t *testing.T) *grpc.ClientConn {
	t.Helper()

	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", grpcAddr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func up() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// --profile auth brings Keycloak in alongside the app profile. --wait blocks until every
	// healthcheck passes, so a realm that fails to import is a startup error here rather than
	// a confusing 401 inside a test.
	out, err := compose(ctx, "--profile", "app", "--profile", "auth",
		"up", "--build", "--wait", "--wait-timeout", "300")
	if err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

func down() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if out, err := compose(ctx, "--profile", "app", "--profile", "auth",
		"down", "-v", "--remove-orphans"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/oidc: teardown failed: %v\n%s\n", err, out)
	}
}

// compose runs docker compose with BOTH files, under this suite's own project name.
func compose(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"compose", "-f", baseFile, "-f", oidcFile, "-p", project}, args...)

	cmd := exec.CommandContext(ctx, "docker", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %w", strings.Join(full, " "), err)
	}
	return string(out), nil
}

func composeExec(t *testing.T, service string, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	out, err := compose(ctx, append([]string{"exec", "-T", service}, args...)...)
	if err != nil {
		t.Fatalf("exec in %s: %v\n%s", service, err, out)
	}
	return out
}

func requireDocker() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		return fmt.Errorf("no Docker daemon; skipping the OIDC e2e tier: %w", err)
	}
	return nil
}

// requireFreePorts guards the collision a separate project name cannot prevent.
//
// This stack publishes the SAME host ports as the sibling e2e package, deliberately: remapping
// seven services would mean the overlay no longer described the shipped stack. The cost is that
// the two packages must not run at once, which is why Taskfile's verify:e2e passes -p 1.
func requireFreePorts() error {
	for _, port := range []string{"50051", "8080", "8081", "9091"} {
		if inUse(port) {
			return fmt.Errorf("host port %s is already bound.\n\n"+
				"This stack publishes the same ports as the rest of the compose file, so a dev "+
				"stack -- or the sibling e2e package running concurrently -- has to stop first. "+
				"Run the tier with `task verify:e2e`, which serialises the packages, or stop "+
				"your own stack:\n\n    task down\n", port)
		}
	}
	return nil
}

func dumpLogs() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	out, _ := compose(ctx, "--profile", "app", "--profile", "auth", "logs", "--tail", "80")
	fmt.Fprintf(os.Stderr, "\n=== compose logs (oidc) ===\n%s\n", out)
}

// inUse reports whether something is already listening on a host port.
func inUse(port string) bool {
	conn, err := net.DialTimeout("tcp", "localhost:"+port, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
