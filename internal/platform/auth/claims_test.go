package auth_test

import (
	"context"
	"slices"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/example/gomicro/internal/platform/auth"
	"github.com/example/gomicro/internal/platform/auth/testjwks"
)

// TestClaimMappingHandlesEveryProviderShape is what stops this template being
// Keycloak-only in practice.
//
// No two identity providers agree on where the tenant and the scopes live, or even on how to
// encode a list. A verifier that hardcodes one vendor's shape works beautifully for one
// reader and is a rewrite for everyone else -- and the rewrite lands in the middle of the
// security-critical path, which is the worst place to invite one.
//
// Each case below is a real token shape from a real provider.
func TestClaimMappingHandlesEveryProviderShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		provider    string
		tenantClaim string
		scopeClaim  string
		claims      func(jwt.MapClaims)
		wantTenant  string
		wantScopes  []string
	}{
		{
			// The shipped default: a custom protocol mapper puts tenant_id at the top level,
			// and OAuth2's own `scope` claim is a space-delimited string.
			provider:    "Keycloak, flat custom claim",
			tenantClaim: "tenant_id",
			scopeClaim:  "scope",
			claims: func(c jwt.MapClaims) {
				c["tenant_id"] = "acme"
				c["scope"] = "orders:read orders:write profile"
			},
			wantTenant: "acme",
			wantScopes: []string{"orders:read", "orders:write", "profile"},
		},
		{
			// Keycloak also exposes roles NESTED under realm_access, as a JSON array. Both
			// the dotted path and the array encoding are needed to read it.
			provider:    "Keycloak, nested realm_access.roles",
			tenantClaim: "tenant_id",
			scopeClaim:  "realm_access.roles",
			claims: func(c jwt.MapClaims) {
				c["tenant_id"] = "acme"
				c["realm_access"] = map[string]any{"roles": []any{"orders:read", "orders:write"}}
			},
			wantTenant: "acme",
			wantScopes: []string{"orders:read", "orders:write"},
		},
		{
			// Auth0 requires custom claims to be namespaced with a URI, which contains dots
			// AND slashes. The literal-key lookup must win over dotted-path splitting, or
			// this claim is unreadable.
			provider:    "Auth0, namespaced URI claim",
			tenantClaim: "https://example.com/tenant_id",
			scopeClaim:  "scope",
			claims: func(c jwt.MapClaims) {
				c["https://example.com/tenant_id"] = "acme"
				c["scope"] = "orders:read"
			},
			wantTenant: "acme",
			wantScopes: []string{"orders:read"},
		},
		{
			// Cognito prefixes custom attributes with "custom:" and puts groups in an array.
			provider:    "Cognito, custom: prefix and groups array",
			tenantClaim: "custom:tenant_id",
			scopeClaim:  "cognito:groups",
			claims: func(c jwt.MapClaims) {
				c["custom:tenant_id"] = "acme"
				c["cognito:groups"] = []any{"orders:read"}
			},
			wantTenant: "acme",
			wantScopes: []string{"orders:read"},
		},
		{
			// Entra ID uses `scp`, space-delimited, and `tid` for the directory tenant.
			provider:    "Entra ID, scp and tid",
			tenantClaim: "tid",
			scopeClaim:  "scp",
			claims: func(c jwt.MapClaims) {
				c["tid"] = "acme"
				c["scp"] = "orders:read orders:write"
			},
			wantTenant: "acme",
			wantScopes: []string{"orders:read", "orders:write"},
		},
		{
			// Defensive: repeated and trailing separators must not produce empty scopes,
			// which would otherwise be a scope that matches an empty rule entry.
			provider:    "sloppy whitespace in a space-delimited scope",
			tenantClaim: "tenant_id",
			scopeClaim:  "scope",
			claims: func(c jwt.MapClaims) {
				c["tenant_id"] = "acme"
				c["scope"] = "  orders:read   orders:write  "
			},
			wantTenant: "acme",
			wantScopes: []string{"orders:read", "orders:write"},
		},
		{
			// A non-string inside a scope array is a provider bug. Skipping the member is
			// right; stringifying it would invent the scope "42", which could match a rule.
			provider:    "array containing a non-string",
			tenantClaim: "tenant_id",
			scopeClaim:  "roles",
			claims: func(c jwt.MapClaims) {
				c["tenant_id"] = "acme"
				c["roles"] = []any{"orders:read", 42, ""}
			},
			wantTenant: "acme",
			wantScopes: []string{"orders:read"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			t.Parallel()

			iss := testjwks.New(t)
			iss.TenantClaim = tc.tenantClaim
			iss.ScopeClaim = tc.scopeClaim

			v := newVerifier(t, iss, func(o *auth.OIDCOptions) {
				o.TenantClaim = tc.tenantClaim
				o.ScopeClaim = tc.scopeClaim
			})

			claims := iss.DefaultClaims()
			// DefaultClaims seeds the configured claim names; clear them so each case
			// builds exactly the token shape it means to.
			delete(claims, tc.tenantClaim)
			delete(claims, tc.scopeClaim)
			tc.claims(claims)

			principal, err := v.Verify(context.Background(), iss.Sign(claims))
			if err != nil {
				t.Fatalf("a valid %s token was rejected: %v", tc.provider, err)
			}
			if principal.TenantID != tc.wantTenant {
				t.Errorf("TenantID = %q, want %q", principal.TenantID, tc.wantTenant)
			}
			if !slices.Equal(principal.Scopes, tc.wantScopes) {
				t.Errorf("Scopes = %v, want %v", principal.Scopes, tc.wantScopes)
			}
		})
	}
}

// TestMissingScopesAreNotAnAuthenticationFailure draws a line that is easy to get backwards.
//
// A token with no scope claim is a valid token held by a caller granted nothing. Rejecting
// it at authentication would refuse a credential that is perfectly good for the public
// surface; accepting it with no scopes lets the default-deny policy do its job, which is
// where that decision belongs.
func TestMissingScopesAreNotAnAuthenticationFailure(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	v := newVerifier(t, iss)

	claims := iss.DefaultClaims()
	delete(claims, iss.ScopeClaim)

	principal, err := v.Verify(context.Background(), iss.Sign(claims))
	if err != nil {
		t.Fatalf("a token with no scopes was rejected outright: %v", err)
	}
	if len(principal.Scopes) != 0 {
		t.Errorf("Scopes = %v, want none", principal.Scopes)
	}
	if principal.TenantID != "tenant-a" {
		t.Errorf("TenantID = %q, want the tenant to still be read", principal.TenantID)
	}
}

// TestServiceTokensAreDistinguishedFromUsers pins IsService against BOTH token shapes,
// including the real one that broke the original implementation.
//
// The first version relied only on RFC 9068 §5 -- in the client credentials grant there is no
// resource owner, so sub IS the client. That is the portable rule and it is what an
// in-process issuer built to the spec produces, so the test passed.
//
// Then M10 booted the shipped Keycloak realm and fetched an actual client-credentials token:
//
//	{"aud":"orderd","azp":"orders-worker","sub":"d57c98ae-746a-450a-ac53-b442bd5780b8",
//	 "scope":"orders:read orders:write","tenant_id":"acme","typ":"Bearer", ...}
//
// No client_id claim at all, and sub is the service account USER's uuid. RFC 9068's rule
// returns false, so every machine caller was classified as an end user -- a latent bug no
// in-process test could ever have found, because the fake was built to the spec the real
// provider does not follow.
func TestServiceTokensAreDistinguishedFromUsers(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	v := newVerifier(t, iss, func(o *auth.OIDCOptions) { o.ServiceClaim = "token_use" })

	cases := []struct {
		name    string
		claims  func(jwt.MapClaims)
		service bool
	}{
		{
			// RFC 9068's shape, for providers that follow it. Costs no configuration.
			name: "RFC 9068: sub equals client_id",
			claims: func(c jwt.MapClaims) {
				c["sub"] = "orders-worker"
				c["client_id"] = "orders-worker"
			},
			service: true,
		},
		{
			// THE REAL KEYCLOAK SHAPE, copied from a token this repo actually fetched.
			name: "Keycloak service account: opaque sub, azp, explicit token_use",
			claims: func(c jwt.MapClaims) {
				c["sub"] = "d57c98ae-746a-450a-ac53-b442bd5780b8"
				c["azp"] = "orders-worker"
				c["token_use"] = "service"
			},
			service: true,
		},
		{
			// The same Keycloak shape WITHOUT the mapper -- what the realm produced before
			// M10 added it. Correctly not a service, and the reason the realm now sets it.
			name: "Keycloak service account with no mapper is not detectable",
			claims: func(c jwt.MapClaims) {
				c["sub"] = "d57c98ae-746a-450a-ac53-b442bd5780b8"
				c["azp"] = "orders-worker"
			},
			service: false,
		},
		{
			// An end user authenticating THROUGH a client still carries azp. That must not
			// make them a service, or every browser login gains machine rights.
			name: "end user with azp is not a service",
			claims: func(c jwt.MapClaims) {
				c["sub"] = "user-123"
				c["azp"] = "orders-web"
			},
			service: false,
		},
		{
			// A claim that exists but says something else must not promote anyone.
			name: "token_use with another value is not a service",
			claims: func(c jwt.MapClaims) {
				c["sub"] = "user-123"
				c["token_use"] = "access"
			},
			service: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			claims := iss.DefaultClaims()
			delete(claims, "azp")
			delete(claims, "client_id")
			tc.claims(claims)

			p, err := v.Verify(context.Background(), iss.Sign(claims))
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if p.IsService != tc.service {
				t.Errorf("IsService = %v, want %v.\n\n"+
					"IsService gates service-only paths; getting it wrong either denies machines "+
					"or hands end users machine rights.", p.IsService, tc.service)
			}
		})
	}
}
