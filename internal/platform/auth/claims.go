package auth

import (
	"fmt"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// Claim mapping is configurable because no two identity providers agree on where anything
// lives, and hardcoding one provider's shape is what makes a "generic" template
// single-vendor in practice.
//
//	                  tenant                              scopes
//	Keycloak          a custom protocol mapper, e.g.      "scope" (space-delimited string)
//	                  "tenant_id", or "realm_access.*"    or "realm_access.roles" (array)
//	Auth0             a namespaced claim, e.g.
//	                  "https://example.com/tenant_id"     "scope" (space-delimited string)
//	Cognito           "custom:tenant_id"                  "cognito:groups" (array)
//	Entra ID          "tid" (the AAD tenant) or an
//	                  app role                            "scp" (space-delimited string)
//
// Hence: dotted paths for nesting, and both the string and array encodings accepted for
// scopes. OIDC_TENANT_CLAIM and OIDC_SCOPE_CLAIM select them; the defaults suit the shipped
// Keycloak realm.

// principalFrom translates verified claims into the domain's identity type.
//
// By the time this runs the signature, issuer, audience and expiry are already checked.
// What remains is a mapping problem -- but a mapping failure is still an auth failure, and
// every path here returns an error rather than a partially-populated Principal.
func (v *OIDCVerifier) principalFrom(claims jwt.MapClaims) (Principal, error) {
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return Principal{}, fmt.Errorf("%w: token has no sub claim", ErrInvalidToken)
	}

	tenant, err := stringClaim(claims, v.opts.TenantClaim)
	if err != nil {
		// A cryptographically valid token that names no tenant cannot be authorised for
		// anything in a multi-tenant service, and failing here -- naming the claim and the
		// fix -- is far better than letting it through to fail later as a puzzling
		// NotFound on data the caller can prove exists.
		return Principal{}, fmt.Errorf(
			"%w: no tenant in claim %q (%v). Add a protocol mapper that puts the tenant id in this claim, "+
				"or set OIDC_TENANT_CLAIM to wherever your provider puts it",
			ErrInvalidToken, v.opts.TenantClaim, err)
	}

	return Principal{
		Subject:   sub,
		TenantID:  tenant,
		Scopes:    scopesClaim(claims, v.opts.ScopeClaim),
		IsService: isServiceToken(claims, sub, v.opts.ServiceClaim),
	}, nil
}

// isServiceToken distinguishes a machine caller from an end user.
//
// TWO RULES, because the standard one does not survive contact with a real provider.
//
// RFC 9068 §5 (JWT Profile for OAuth 2.0 Access Tokens) says that in the client credentials
// grant there is no resource owner, so `sub` IS the client -- which makes `sub == client_id`
// the portable test. That was the only rule here until the shipped Keycloak realm was
// actually booted in M10, and a genuine client-credentials token turned out to carry:
//
//	{"aud":"orderd","azp":"orders-worker","sub":"d57c98ae-...","tenant_id":"acme", ...}
//
// No `client_id` claim at all, and `sub` is the service account USER's uuid rather than the
// client id. RFC 9068's rule returns false, so every machine caller would have been
// classified as an end user. Nothing in this repo reads IsService yet, so it was a latent
// bug -- and one that no amount of in-process testing would ever have found, because the
// in-process issuer was built to the spec the real provider does not follow.
//
// So the second rule is an EXPLICIT claim, configured like every other claim mapping in this
// package. The shipped realm sets it with a hardcoded protocol mapper; a provider that does
// follow RFC 9068 needs no configuration at all.
func isServiceToken(claims jwt.MapClaims, sub, serviceClaim string) bool {
	// Rule 1: RFC 9068. Free when the provider follows it.
	for _, key := range []string{"client_id", "azp"} {
		if id, ok := claims[key].(string); ok && id != "" && id == sub {
			return true
		}
	}

	// Rule 2: an explicit claim, for providers that do not.
	if serviceClaim == "" {
		return false
	}
	val, err := stringClaim(claims, serviceClaim)
	if err != nil {
		return false
	}
	return val == ServiceTokenValue
}

// ServiceTokenValue is what OIDC_SERVICE_CLAIM must contain to mark a machine caller.
//
// A fixed value rather than "any non-empty value", so a claim that happens to exist for an
// unrelated reason cannot silently promote every end user to a service.
const ServiceTokenValue = "service"

// stringClaim reads a string claim, following a dotted path into nested objects.
func stringClaim(claims jwt.MapClaims, path string) (string, error) {
	raw, ok := lookupClaim(claims, path)
	if !ok {
		return "", fmt.Errorf("claim not present")
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("claim is %T, want string", raw)
	}
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("claim is empty")
	}
	return s, nil
}

// scopesClaim reads granted scopes in either encoding providers use.
//
// A missing or malformed scope claim yields NO scopes rather than an error. That is the
// fail-closed direction here: with the default-deny policy, a principal holding no scopes
// can reach only rules that require none. Erroring instead would reject a token that is
// perfectly valid for the public surface.
func scopesClaim(claims jwt.MapClaims, path string) []string {
	raw, ok := lookupClaim(claims, path)
	if !ok {
		return nil
	}

	switch val := raw.(type) {
	case string:
		// OAuth 2.0 (RFC 6749 §3.3) form: space-delimited. Fields, not Split, so repeated
		// or trailing spaces do not produce empty scopes.
		return strings.Fields(val)

	case []string:
		return val

	case []any:
		// JSON arrays decode to []any. Non-string members are skipped rather than
		// stringified: a scope list containing a number is a provider bug, and inventing
		// the scope "42" from it could match a rule.
		out := make([]string, 0, len(val))
		for _, item := range val {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out

	default:
		return nil
	}
}

// lookupClaim resolves a possibly-dotted path, e.g. "realm_access.roles".
//
// A literal claim name containing dots -- which Auth0 namespaced claims and Cognito's
// "custom:" prefix can both produce -- is tried first, so "https://example.com/tenant_id"
// works without escaping.
func lookupClaim(claims jwt.MapClaims, path string) (any, bool) {
	if path == "" {
		return nil, false
	}
	if v, ok := claims[path]; ok {
		return v, true
	}

	parts := strings.Split(path, ".")
	if len(parts) == 1 {
		return nil, false
	}

	var current any = map[string]any(claims)
	for _, part := range parts {
		obj, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = obj[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}
