package auth

import "context"

// DevPrincipal is the identity every request runs as when AUTH_MODE=dev.
//
// It exists for exactly one reason: so `git clone && go run ./cmd/orderd` works with no
// identity provider, no keys, and no configuration. That is the difference between a
// template someone tries and a template someone closes.
//
// It is guarded in three places, because an accidental production deploy with dev auth is
// a total authentication bypass:
//
//   - config.Validate refuses AUTH_MODE=dev when APP_ENV=production, before any listener
//     opens (config_test.go::TestValidateRejectsDevAuthInProduction).
//   - NewVerifier logs a WARN on every single startup, not once.
//   - the deploy overlay sets AUTH_MODE=oidc explicitly rather than relying on a default.
var DevPrincipal = Principal{
	Subject:  "dev-user",
	TenantID: "dev-tenant",
	Scopes:   []string{"orders:read", "orders:write"},
}

// DevVerifier authenticates NOBODY. It returns DevPrincipal for any input, including the
// empty string.
//
// It is not a "weak" verifier that could be hardened; it performs no verification at all,
// and the name says so. A type called something like StaticVerifier or SimpleVerifier
// invites someone to reach for it in a staging environment.
type DevVerifier struct{}

// Verify ignores its argument entirely and always succeeds.
//
// Accepting the empty token is deliberate rather than sloppy: requiring some placeholder
// string would make dev mode look like it checks something, and would mean the quickstart
// has to explain a fake credential. Failing to authenticate is the honest behaviour to
// document, and the honest behaviour to name.
func (DevVerifier) Verify(context.Context, string) (Principal, error) {
	return DevPrincipal, nil
}
