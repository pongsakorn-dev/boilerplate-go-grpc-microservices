package auth_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/example/gomicro/internal/platform/auth"
	"github.com/example/gomicro/internal/platform/auth/testjwks"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newVerifier builds a verifier pointed at an in-process issuer.
func newVerifier(t *testing.T, iss *testjwks.Issuer, mutate ...func(*auth.OIDCOptions)) auth.Verifier {
	t.Helper()

	opts := auth.OIDCOptions{
		IssuerURL:   iss.URL(),
		Audience:    iss.Audience,
		Leeway:      0,
		TenantClaim: iss.TenantClaim,
		ScopeClaim:  iss.ScopeClaim,
		HTTPClient:  iss.Client(),
		Log:         discardLogger(),
	}
	for _, fn := range mutate {
		fn(&opts)
	}

	v, err := auth.NewOIDCVerifier(opts)
	if err != nil {
		t.Fatalf("NewOIDCVerifier: %v", err)
	}
	return v
}

// TestVerifyAcceptsAWellFormedToken is the baseline every rejection test depends on.
//
// Without it, a verifier that rejects EVERYTHING would pass the entire hostile-token suite
// below with flying colours. That is not a hypothetical failure mode -- it is the single
// most likely way for a security test suite to be worthless.
func TestVerifyAcceptsAWellFormedToken(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	v := newVerifier(t, iss)

	for _, tc := range []struct {
		name string
		sign func(jwt.MapClaims) string
	}{
		{"ES256", iss.Sign},
		{"RS256", iss.SignRSA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			principal, err := v.Verify(context.Background(), tc.sign(iss.DefaultClaims()))
			if err != nil {
				t.Fatalf("a valid %s token was rejected: %v", tc.name, err)
			}

			if principal.Subject != "user-123" {
				t.Errorf("Subject = %q, want user-123", principal.Subject)
			}
			// The rule this whole package exists to enforce: the tenant comes from the
			// verified token, never from the request body.
			if principal.TenantID != "tenant-a" {
				t.Errorf("TenantID = %q, want tenant-a", principal.TenantID)
			}
			if !principal.HasScope("orders:read") || !principal.HasScope("orders:write") {
				t.Errorf("Scopes = %v, want both orders:read and orders:write", principal.Scopes)
			}
		})
	}
}

// TestVerifyRejectsHostileTokens is the reason testjwks exists.
//
// Every case is a token a real attacker sends, and every one of them verifies
// cryptographically against SOMETHING -- which is what makes them dangerous. A verifier
// that checks only "is the signature valid" passes several of these.
func TestVerifyRejectsHostileTokens(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		why  string
		mint func(*testjwks.Issuer) string
	}{
		{
			name: "alg none",
			why: "the original JWT vulnerability: an unsigned token. A verifier that reads " +
				"the algorithm from the token and dispatches on it accepts a token anyone can write.\n\n" +
				"Like the HS256 case below, most of the credit here belongs to golang-jwt, which " +
				"refuses \"none\" unless handed the explicit UnsafeAllowNoneSignatureType sentinel. " +
				"Our allowlist is the second line. Kept as an end-to-end regression test rather " +
				"than a proof that our own code is what rejects it.",
			mint: func(i *testjwks.Issuer) string { return i.SignNone(i.DefaultClaims()) },
		},
		{
			name: "HS256 MACed with the issuer's RSA public key",
			why: "ALGORITHM CONFUSION. The attacker cannot sign with the private key, so they " +
				"use the PUBLIC key -- published in the JWKS for anyone to read -- as an HMAC " +
				"secret. A verifier that picks its algorithm from the token header validates it " +
				"against those same bytes and succeeds.\n\n" +
				"HONEST SCOPE OF THIS CASE: it is a regression test, not a proof that our own " +
				"checks work. Deleting both the allowlist and keyFunc's key-type switch leaves it " +
				"passing, because the key cache refuses symmetric keys outright and golang-jwt's " +
				"HMAC verifier rejects a typed *rsa.PublicKey before either of ours runs. That was " +
				"established by deliberately removing them. It stays because it pins the end-to-end " +
				"behaviour against a future change to any of those layers -- and see " +
				"TestKeyAlgorithmBindingIsEnforced for the substitution our code alone catches.",
			mint: func(i *testjwks.Issuer) string { return i.SignHS256WithRSAPublicKey(i.DefaultClaims()) },
		},
		{
			name: "unknown key id",
			why:  "a signature from a key this issuer never published",
			mint: func(i *testjwks.Issuer) string { return i.SignWithKid("attacker-key", i.DefaultClaims()) },
		},
		{
			name: "expired",
			why:  "a leaked token must stop working",
			mint: func(i *testjwks.Issuer) string {
				c := i.DefaultClaims()
				c["exp"] = time.Now().Add(-time.Hour).Unix()
				return i.Sign(c)
			},
		},
		{
			name: "no expiry at all",
			why: "a token with no exp never expires. Requiring exp is what bounds the blast " +
				"radius of a leak; without it a single stolen token is permanent.",
			mint: func(i *testjwks.Issuer) string {
				c := i.DefaultClaims()
				delete(c, "exp")
				return i.Sign(c)
			},
		},
		{
			name: "not yet valid",
			why:  "nbf in the future",
			mint: func(i *testjwks.Issuer) string {
				c := i.DefaultClaims()
				c["nbf"] = time.Now().Add(time.Hour).Unix()
				return i.Sign(c)
			},
		},
		{
			name: "wrong audience",
			why: "a token minted for a DIFFERENT application on the same identity provider. " +
				"This is the failure that silently grants another product's users access to yours.",
			mint: func(i *testjwks.Issuer) string {
				c := i.DefaultClaims()
				c["aud"] = "some-other-service"
				return i.Sign(c)
			},
		},
		{
			name: "wrong issuer",
			why:  "correct shape, wrong authority",
			mint: func(i *testjwks.Issuer) string {
				c := i.DefaultClaims()
				c["iss"] = "https://evil.example.com"
				return i.Sign(c)
			},
		},
		{
			name: "no subject",
			why:  "an authenticated caller with no identity cannot be audited or rate limited",
			mint: func(i *testjwks.Issuer) string {
				c := i.DefaultClaims()
				delete(c, "sub")
				return i.Sign(c)
			},
		},
		{
			name: "no tenant claim",
			why: "cryptographically perfect and useless: a multi-tenant service cannot authorise " +
				"a caller who belongs to no tenant. Failing here, naming the claim, beats failing " +
				"later as a baffling NotFound on data the caller can prove exists.",
			mint: func(i *testjwks.Issuer) string {
				c := i.DefaultClaims()
				delete(c, i.TenantClaim)
				return i.Sign(c)
			},
		},
		{
			name: "structurally garbage",
			why:  "not a JWT at all",
			mint: func(*testjwks.Issuer) string { return "not.a.token" },
		},
		{
			name: "empty string",
			why:  "no credential presented",
			mint: func(*testjwks.Issuer) string { return "" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			iss := testjwks.New(t)
			v := newVerifier(t, iss)

			principal, err := v.Verify(context.Background(), tc.mint(iss))
			if err == nil {
				t.Fatalf("ACCEPTED a token that must be rejected.\n\nWhy this matters: %s\n\n"+
					"It authenticated as subject=%q tenant=%q scopes=%v",
					tc.why, principal.Subject, principal.TenantID, principal.Scopes)
			}

			// Every failure must be one of the two sentinels, so the interceptor can map
			// them without string matching.
			if !errors.Is(err, auth.ErrInvalidToken) && !errors.Is(err, auth.ErrNoCredentials) {
				t.Errorf("error is neither ErrInvalidToken nor ErrNoCredentials: %v", err)
			}
		})
	}
}

// TestKeyAlgorithmBindingIsEnforced covers the one algorithm substitution that our code, and
// only our code, rejects.
//
// The issuer publishes its RSA key with `"alg": "RS256"`. A PS256 token signed with the same
// private key verifies perfectly -- same key, different padding scheme -- so no signature
// check catches it and golang-jwt has no opinion. Only the JWKS's own declaration says this
// key was not meant for that, and keyFunc enforces it.
//
// This test exists because the HS256 case above turned out to prove nothing about our code.
// Deleting the alg-binding check in keyFunc makes THIS one fail, which is the property a
// security test needs to have.
func TestKeyAlgorithmBindingIsEnforced(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	v := newVerifier(t, iss)

	// Baseline: the same key, used for the algorithm it was published for, works.
	if _, err := v.Verify(context.Background(), iss.SignRSA(iss.DefaultClaims())); err != nil {
		t.Fatalf("RS256 with the RS256 key was rejected, so this test proves nothing: %v", err)
	}

	if _, err := v.Verify(context.Background(), iss.SignPS256(iss.DefaultClaims())); err == nil {
		t.Fatal("accepted a PS256 token signed with a key the JWKS publishes as RS256.\n\n" +
			"The signature is valid -- it is the same RSA key -- so nothing below this verifier " +
			"will catch it. The issuer's own `alg` declaration is the only thing that says this " +
			"key was not meant for this algorithm, and it must be enforced.")
	}
}

// TestVerifyRejectsATokenSignedByADifferentIssuersKey is the attack the discovery
// issuer-match check exists to stop, run end to end.
//
// Two issuers, both perfectly valid. A token from the second is presented to a verifier
// trusting the first. It is correctly signed, unexpired, and names the right audience -- the
// only thing wrong with it is who signed it, which is the entire question.
func TestVerifyRejectsATokenSignedByADifferentIssuersKey(t *testing.T) {
	t.Parallel()

	trusted := testjwks.New(t)
	other := testjwks.New(t)

	v := newVerifier(t, trusted)

	// Claim to be the trusted issuer, but sign with the other issuer's key.
	claims := other.DefaultClaims()
	claims["iss"] = trusted.URL()
	claims["aud"] = trusted.Audience

	if _, err := v.Verify(context.Background(), other.Sign(claims)); err == nil {
		t.Fatal("accepted a token signed by an issuer this service does not trust")
	}
}

// TestLeewayToleratesClockSkewButNotExpiry draws the line the Leeway knob controls.
//
// Clock skew between a service and its IdP is real and routine; rejecting a token that
// expired 400ms ago because of NTP drift produces intermittent, unreproducible auth failures.
// Leeway exists for that. What it must NOT do is turn into a general grace period on expiry.
func TestLeewayToleratesClockSkewButNotExpiry(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	v := newVerifier(t, iss, func(o *auth.OIDCOptions) { o.Leeway = 30 * time.Second })

	t.Run("within skew is accepted", func(t *testing.T) {
		c := iss.DefaultClaims()
		c["exp"] = time.Now().Add(-5 * time.Second).Unix()
		if _, err := v.Verify(context.Background(), iss.Sign(c)); err != nil {
			t.Errorf("a token 5s past expiry was rejected with 30s of leeway: %v", err)
		}
	})

	t.Run("beyond skew is rejected", func(t *testing.T) {
		c := iss.DefaultClaims()
		c["exp"] = time.Now().Add(-5 * time.Minute).Unix()
		if _, err := v.Verify(context.Background(), iss.Sign(c)); err == nil {
			t.Error("a token 5 minutes past expiry was accepted; leeway is acting as a grace period")
		}
	})
}

// TestNewOIDCVerifierRefusesUnsafeConfiguration checks the failures that happen before any
// token exists -- at construction, which is at startup, which is before a listener binds.
func TestNewOIDCVerifierRefusesUnsafeConfiguration(t *testing.T) {
	t.Parallel()

	valid := auth.OIDCOptions{
		IssuerURL:   "https://issuer.example.com",
		Audience:    "orderd",
		TenantClaim: "tenant_id",
		Log:         discardLogger(),
	}

	cases := []struct {
		name    string
		mutate  func(*auth.OIDCOptions)
		wantHas string
	}{
		{
			name:   "no audience",
			mutate: func(o *auth.OIDCOptions) { o.Audience = "" },
			// Not defaulted, refused. A verifier with no expected audience accepts every
			// token the issuer ever minted, for any application sharing that IdP.
			wantHas: "OIDC_AUDIENCE",
		},
		{
			name:    "no issuer",
			mutate:  func(o *auth.OIDCOptions) { o.IssuerURL = "" },
			wantHas: "OIDC_ISSUER_URL",
		},
		{
			name:    "no tenant claim",
			mutate:  func(o *auth.OIDCOptions) { o.TenantClaim = "" },
			wantHas: "OIDC_TENANT_CLAIM",
		},
		{
			name: "plaintext issuer on a real host",
			// Bearer tokens and signing keys over cleartext are one wiretap from total
			// compromise. Loopback is exempt so the test suite and a laptop pointed at
			// compose can work; a real hostname is not.
			mutate:  func(o *auth.OIDCOptions) { o.IssuerURL = "http://issuer.example.com" },
			wantHas: "cleartext",
		},
		{
			name:    "plaintext jwks override on a real host",
			mutate:  func(o *auth.OIDCOptions) { o.JWKSURL = "http://keys.example.com/jwks" },
			wantHas: "cleartext",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := valid
			tc.mutate(&opts)

			_, err := auth.NewOIDCVerifier(opts)
			if err == nil {
				t.Fatal("this configuration was accepted; it must fail at startup, not at the first request")
			}
			if !strings.Contains(err.Error(), tc.wantHas) {
				t.Errorf("error does not mention %q: %v", tc.wantHas, err)
			}
		})
	}
}

// TestLoopbackPlaintextIsAllowed is the other side of the previous test, and it protects the
// quickstart: without this exemption the entire suite, and any laptop pointed at a
// compose-hosted Keycloak, would need TLS.
func TestLoopbackPlaintextIsAllowed(t *testing.T) {
	t.Parallel()

	for _, url := range []string{"http://localhost:8080/realms/x", "http://127.0.0.1:8080/realms/x"} {
		if _, err := auth.NewOIDCVerifier(auth.OIDCOptions{
			IssuerURL:   url,
			Audience:    "orderd",
			TenantClaim: "tenant_id",
			Log:         discardLogger(),
		}); err != nil {
			t.Errorf("%s was refused: %v", url, err)
		}
	}
}
