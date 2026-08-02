package client_test

import (
	"strings"
	"testing"

	"github.com/example/gomicro/internal/platform/client"
	"github.com/example/gomicro/internal/platform/config"
)

// TestInsecureIsRefusedInProduction stops the most common way a template's convenience becomes
// a fork's vulnerability.
//
// Insecure transport is genuinely correct for bufconn and a local stack, so it must be
// available -- and every reachable convenience is eventually copied into a production
// manifest. An unencrypted service-to-service call carries this service's own credential in
// clear text, so anything on the path can lift it and impersonate the service.
//
// It cannot be reached by omission either: leaving TransportCredentials nil means TLS with the
// system roots, not insecure. You have to ask for it by name.
func TestInsecureIsRefusedInProduction(t *testing.T) {
	t.Parallel()

	prod, err := config.Parse(map[string]string{
		"APP_ENV":         config.EnvProduction,
		"AUTH_MODE":       config.AuthOIDC,
		"OIDC_ISSUER_URL": "https://issuer.example.com/realms/gomicro",
		"OIDC_AUDIENCE":   "orderd",
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	opts := client.New(prod, "passthrough:///upstream")
	opts.TransportCredentials = client.Insecure()

	conn, err := client.Dial(prod, opts)
	if err == nil {
		_ = conn.Close()
		t.Fatal("an insecure upstream connection was allowed under APP_ENV=production")
	}
	if !strings.Contains(err.Error(), "clear text") {
		t.Errorf("the error does not explain the risk: %v", err)
	}

	// The same options are fine in development, which is what makes this a guard rather than
	// an obstacle.
	dev := testConfig(t)
	devOpts := client.New(dev, "passthrough:///upstream")
	devOpts.TransportCredentials = client.Insecure()
	devConn, err := client.Dial(dev, devOpts)
	if err != nil {
		t.Fatalf("insecure was refused in development too: %v", err)
	}
	_ = devConn.Close()
}

// TestTheDefaultIsTLS covers the omission case: forgetting to think about transport security
// must not produce a plaintext connection.
func TestTheDefaultIsTLS(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	opts := client.New(cfg, "passthrough:///upstream") // TransportCredentials deliberately unset

	conn, err := client.Dial(cfg, opts)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// grpc.NewClient is lazy, so nothing has been negotiated yet. What is asserted here is
	// that Dial did not quietly substitute insecure credentials -- if it had, the identical
	// call under APP_ENV=production would have been refused by the guard above.
	prod, err := config.Parse(map[string]string{
		"APP_ENV":         config.EnvProduction,
		"AUTH_MODE":       config.AuthOIDC,
		"OIDC_ISSUER_URL": "https://issuer.example.com/realms/gomicro",
		"OIDC_AUDIENCE":   "orderd",
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	prodConn, err := client.Dial(prod, client.New(prod, "passthrough:///upstream"))
	if err != nil {
		t.Fatalf("the default credentials were refused in production, so they are insecure: %v", err)
	}
	_ = prodConn.Close()
}
