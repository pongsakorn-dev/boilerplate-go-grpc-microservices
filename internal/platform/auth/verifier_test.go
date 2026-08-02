package auth_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/platform/auth"
	"github.com/example/gomicro/internal/platform/config"
)

// TestNewVerifierRefusesAnUnknownMode is the guard for the bug this repo actually shipped.
//
// The server once installed the dev interceptor unconditionally and never read AUTH_MODE, so
// setting it to `oidc` was silently ignored: the service booted clean, reported healthy, and
// served every request as a full-scope dev principal. The bypass was confirmed by calling an
// RPC with no credentials and getting three orders back.
//
// The structural lesson is narrow and worth stating: a factory that cannot express "I do not
// recognise this mode" will fall through to whatever it can build, and what it can build is
// always the permissive one. So the default arm errors, and this test holds it there.
func TestNewVerifierRefusesAnUnknownMode(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"", "none", "disabled", "off", "DEV", "OIDC", "jwt"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			cfg := config.Config{AuthMode: mode}
			v, err := auth.NewVerifier(cfg, discardLogger())

			if err == nil {
				t.Fatalf("AUTH_MODE=%q produced a verifier instead of an error. If that verifier "+
					"is permissive, this is an authentication bypass that looks like configuration.", mode)
			}
			// Never a usable verifier alongside an error: a caller that checks the error
			// loosely, or logs and continues, must not find something callable here.
			if v != nil {
				t.Errorf("AUTH_MODE=%q returned both an error and a non-nil verifier", mode)
			}
		})
	}
}

// TestDevModeWarnsOnEveryStartup keeps the loudest guard against dev auth in production.
//
// Warning once at first boot is not enough: in an orchestrator that restarts pods constantly
// the line scrolls away within minutes, and the whole purpose is that somebody notices it in
// an environment where it should not appear.
func TestDevModeWarnsOnEveryStartup(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	for range 3 {
		if _, err := auth.NewVerifier(config.Config{AuthMode: config.AuthDev}, log); err != nil {
			t.Fatalf("dev mode failed to build: %v", err)
		}
	}

	if got := strings.Count(buf.String(), "AUTH_MODE=dev"); got != 3 {
		t.Errorf("got %d warnings from 3 startups, want 3 -- a once-only warning is one nobody sees", got)
	}
	// The warning has to say what to do about it, not merely that something is true.
	if !strings.Contains(buf.String(), "AUTH_MODE=oidc") {
		t.Error("the dev-mode warning does not name the remedy")
	}
}

// TestDevVerifierAuthenticatesNobody pins the honest behaviour of dev mode, including that
// it accepts the empty token.
//
// Accepting "" is deliberate. Requiring some placeholder credential would make dev mode look
// like it checks something and would force the quickstart to explain a fake token. The
// property worth documenting -- and testing -- is that it verifies nothing at all.
func TestDevVerifierAuthenticatesNobody(t *testing.T) {
	t.Parallel()

	v, err := auth.NewVerifier(config.Config{AuthMode: config.AuthDev}, discardLogger())
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	for _, token := range []string{"", "garbage", "Bearer nonsense", strings.Repeat("x", 5000)} {
		p, err := v.Verify(context.Background(), token)
		if err != nil {
			t.Errorf("dev verifier rejected %q; it is documented as verifying nothing: %v", token, err)
		}
		if p.TenantID != auth.DevPrincipal.TenantID {
			t.Errorf("dev verifier returned tenant %q, want %q", p.TenantID, auth.DevPrincipal.TenantID)
		}
	}
}

// TestOIDCModeBuildsARealVerifier is the assertion that would have caught a leftover refusal
// after the verifier landed -- the mirror image of the bug above.
func TestOIDCModeBuildsARealVerifier(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		AuthMode: config.AuthOIDC,
		OIDC: config.OIDCConfig{
			IssuerURL:   "https://issuer.example.com/realms/gomicro",
			Audience:    "orderd",
			TenantClaim: "tenant_id",
			ScopeClaim:  "scope",
		},
	}

	v, err := auth.NewVerifier(cfg, discardLogger())
	if err != nil {
		t.Fatalf("a fully configured OIDC mode failed to build: %v", err)
	}
	if _, isDev := v.(auth.DevVerifier); isDev {
		t.Fatal("AUTH_MODE=oidc produced the DEV verifier. This is the exact bypass this " +
			"package was rewritten to prevent: a service that looks configured and authenticates nobody.")
	}
}
