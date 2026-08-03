package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/example/gomicro/internal/platform/config"
)

// Verifier turns a raw bearer credential into a verified Principal.
//
// The interface is deliberately one method. Everything a verifier needs to decide -- the
// issuer, the audience, the key set, the clock skew allowance -- is fixed at construction,
// so a caller cannot accidentally relax a check per-request.
type Verifier interface {
	// Verify returns the Principal a credential proves. It must return an error rather
	// than a zero Principal when verification fails: a zero Principal is
	// indistinguishable from "an authenticated caller with no tenant and no scopes",
	// which several call sites would happily proceed with.
	Verify(ctx context.Context, rawToken string) (Principal, error)
}

// Sentinel errors. The interceptor maps all of them to codes.Unauthenticated and never
// forwards the detail to the client -- "expired at 14:02" and "unknown key id" are useful
// to an operator reading logs and useful to an attacker probing the surface.
var (
	// ErrNoCredentials means no bearer token was presented at all.
	ErrNoCredentials = errors.New("no credentials presented")

	// ErrInvalidToken means a token was presented and did not verify. Callers must not
	// distinguish the reasons to the client.
	ErrInvalidToken = errors.New("token verification failed")
)

// NewVerifier selects the verifier for the configured AUTH_MODE.
//
// The default arm returns an error, never a permissive fallback, and never a nil Verifier.
// That is the entire point of this function existing rather than a switch inline at the
// call site.
//
// This repo has already been bitten by the alternative. An earlier build wired the dev
// interceptor unconditionally and never read AUTH_MODE, so a deployment with
// AUTH_MODE=oidc and APP_ENV=production booted clean and served every request as a
// full-scope dev principal. The bypass was confirmed by test, not by inspection. A factory
// that cannot express "I do not recognise this mode" is how that happens, so this one can.
func NewVerifier(cfg config.Config, log *slog.Logger) (Verifier, error) {
	switch cfg.AuthMode {
	case config.AuthDev:
		// Loud on EVERY startup, not once. A warning printed only on the first boot of a
		// pod is a warning nobody sees in a system that restarts pods constantly.
		log.Warn("AUTH_MODE=dev: every request is served as a fixed principal and NO credential is verified",
			slog.String("subject", DevPrincipal.Subject),
			slog.String("tenant", DevPrincipal.TenantID),
			slog.String("remedy", "set AUTH_MODE=oidc with OIDC_ISSUER_URL and OIDC_AUDIENCE before exposing this to any untrusted network"))
		return DevVerifier{}, nil

	case config.AuthOIDC:
		// Loud on EVERY startup, same as AUTH_MODE=dev above and for the same reason.
		//
		// This one is easier to miss because everything else about the service is correct:
		// tokens really are verified, signatures really are checked, and the only thing given
		// up is the confidentiality of the channel carrying them. That makes it precisely the
		// setting somebody copies out of a compose file into a manifest without noticing.
		// APP_ENV=production refuses it outright; this covers staging and everywhere else.
		if cfg.OIDC.AllowInsecureIssuer {
			log.Warn("OIDC_ALLOW_INSECURE_ISSUER=true: tokens and signing keys may cross this network in cleartext",
				slog.String("issuer", cfg.OIDC.IssuerURL),
				slog.String("remedy", "unset it and give the issuer a TLS certificate before this reaches any network you do not control"))
		}

		return NewOIDCVerifier(OIDCOptions{
			IssuerURL:           cfg.OIDC.IssuerURL,
			Audience:            cfg.OIDC.Audience,
			JWKSURL:             cfg.OIDC.JWKSURL,
			Leeway:              cfg.OIDC.Leeway,
			TenantClaim:         cfg.OIDC.TenantClaim,
			ScopeClaim:          cfg.OIDC.ScopeClaim,
			ServiceClaim:        cfg.OIDC.ServiceClaim,
			MaxKeyAge:           cfg.OIDC.MaxKeyAge,
			AllowInsecureIssuer: cfg.OIDC.AllowInsecureIssuer,
			Log:                 log,
		})

	default:
		// Unreachable through config.Parse, which validates AuthMode. Reachable by any
		// caller constructing a Config literally -- which every test does.
		return nil, fmt.Errorf("unknown AUTH_MODE %q: no verifier can be built, refusing to start", cfg.AuthMode)
	}
}
