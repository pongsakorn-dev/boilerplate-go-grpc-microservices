package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// OIDCOptions configures the OIDC verifier.
//
// A struct rather than eight parameters, because six of them are strings and a call site
// that transposes two of them still compiles.
type OIDCOptions struct {
	// IssuerURL is the `iss` every token must carry. It is also where discovery happens
	// when JWKSURL is empty.
	IssuerURL string

	// Audience is the `aud` every token must contain.
	//
	// This is the single most common OIDC integration failure, and it is worth stating
	// plainly: a Keycloak access token's default audience is "account", NOT your API. You
	// must add an Audience protocol mapper to the client, or every request here fails with
	// "invalid audience". deploy/keycloak/realm-export.json ships that mapper configured.
	Audience string

	// JWKSURL overrides discovery. Empty means "fetch it from IssuerURL", which is the
	// normal path and the reason a fork only needs to set two variables.
	JWKSURL string

	// Leeway tolerates clock skew on exp/nbf/iat.
	Leeway time.Duration

	// TenantClaim is where the tenant id lives. Supports dotted paths for nested claims,
	// e.g. "realm_access.tenant".
	TenantClaim string

	// ServiceClaim marks a machine caller. Empty means RFC 9068's sub==client_id rule only,
	// which real Keycloak service accounts do NOT satisfy -- see auth/claims.go.
	ServiceClaim string

	// ScopeClaim is where granted scopes live. Accepts either the OAuth2 space-delimited
	// string form or a JSON array; providers disagree and both are common.
	ScopeClaim string

	// HTTPClient is injectable so tests drive an in-process issuer. Nil means a sane
	// default with a timeout -- never http.DefaultClient, which has none, so a hung IdP
	// would pin a goroutine and an admission slot until the caller's deadline.
	HTTPClient *http.Client

	// MaxKeyAge bounds how long a cached key set is served before revalidation. Zero means
	// the default (15 minutes); negative disables expiry, which means REVOCATION NEVER TAKES
	// EFFECT and only a test should do it.
	MaxKeyAge time.Duration

	// MinRefresh rate-limits rotation-triggered JWKS refetches. Zero means the default.
	//
	// A NEGATIVE value disables rate limiting entirely. Only tests should do that, and the
	// affordance exists because there is no small-but-positive value that works: this
	// repo's Windows development machine returns exactly 0 from time.Since for two
	// successive time.Now calls, so a 1ns limit is indistinguishable from an infinite one
	// and a rotation test hangs on a clock artefact rather than on the behaviour it means
	// to check. Discovered by writing the rotation test and watching it fail.
	MinRefresh time.Duration

	Log *slog.Logger
}

// signingAlgorithms is the allowlist, and its most important property is what is ABSENT.
//
// No HS*. No "none". Both exclusions are load-bearing:
//
//   - "none" means the token is unsigned. A verifier that accepts it accepts anything.
//   - HS* is symmetric, which is the classic algorithm-confusion attack: the attacker takes
//     the issuer's PUBLIC RSA key (published in the JWKS, readable by everyone), MACs a
//     token of their choosing with it as an HMAC secret, and sets alg:HS256. A verifier
//     that picks its algorithm from the token header validates it happily, because the
//     "secret" and the public key are the same bytes.
//
// HOW MUCH OF THIS DEFENCE IS ACTUALLY OURS -- stated precisely, because the first version
// of this comment overclaimed and a sabotage run caught it.
//
// The structural defence is in jwks.go, not here: the key cache REFUSES symmetric keys, so
// no HMAC algorithm can ever obtain a usable key from it, whatever the token header asks
// for. Underneath that, golang-jwt/jwt/v5's HMAC verifier requires a []byte key and is
// handed a typed *rsa.PublicKey, so it rejects the classic attack on type alone.
//
// This allowlist and keyFunc's key-type switch are defence in depth on top of those two.
// Deleting BOTH does NOT make the classic HS256-with-the-RSA-public-key attack succeed --
// established by deleting them and re-running the suite, which stayed green. They earn their
// place by making the intent explicit and by catching what the library cannot: an algorithm
// substitution WITHIN a key type, e.g. a key published for RS256 used to verify PS256. That
// one is caught by the alg binding in keyFunc and by nothing else, and
// TestKeyAlgorithmBindingIsEnforced fails when it is removed.
var signingAlgorithms = []string{
	"RS256", "RS384", "RS512",
	"PS256", "PS384", "PS512",
	"ES256", "ES384", "ES512",
}

// OIDCVerifier verifies JWT bearer tokens against an OIDC issuer's published keys.
type OIDCVerifier struct {
	opts   OIDCOptions
	log    *slog.Logger
	client *http.Client

	// Discovery is lazy, and only SUCCESS is memoised.
	//
	// Lazy because doing it in the constructor makes the service fail to start when the IdP
	// is briefly unreachable, turning an IdP blip into a deployment that cannot roll out --
	// worse than a few seconds of rejected requests.
	//
	// Success-only because the obvious sync.Once version is an operational trap. A pod whose
	// very first request happens to arrive while the IdP is restarting would cache that
	// error for the life of the process and never authenticate anyone again. Nothing would
	// recover it either: the liveness probe reports process health by design, so Kubernetes
	// never restarts the pod, and it sits there serving Unauthenticated forever. Retries are
	// rate-limited by MinRefresh, so an outage still cannot become a request-rate hammer
	// pointed at the IdP.
	mu            sync.RWMutex
	lastDiscovery time.Time
	keys          *keyCache
	discoverErr   error
}

// NewOIDCVerifier validates configuration eagerly and defers all network access.
func NewOIDCVerifier(opts OIDCOptions) (*OIDCVerifier, error) {
	var errs []error
	if opts.IssuerURL == "" {
		errs = append(errs, errors.New("OIDC_ISSUER_URL is required when AUTH_MODE=oidc"))
	}
	if opts.Audience == "" {
		// Refused rather than defaulted. A verifier with no expected audience accepts any
		// token the issuer ever minted -- including one issued to a completely different
		// application that happens to share the IdP. That is a real and common breach.
		errs = append(errs, errors.New("OIDC_AUDIENCE is required when AUTH_MODE=oidc: "+
			"without it this service would accept any token the issuer signed, including tokens meant for other applications"))
	}
	if opts.TenantClaim == "" {
		errs = append(errs, errors.New("OIDC_TENANT_CLAIM is required: the tenant must come from the verified token"))
	}
	if err := requireSecureURL(opts.IssuerURL, "OIDC_ISSUER_URL"); opts.IssuerURL != "" && err != nil {
		errs = append(errs, err)
	}
	if err := requireSecureURL(opts.JWKSURL, "OIDC_JWKS_URL"); opts.JWKSURL != "" && err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if opts.MinRefresh == 0 {
		opts.MinRefresh = 30 * time.Second
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	return &OIDCVerifier{opts: opts, log: log, client: client}, nil
}

// Verify checks the token and returns the caller it proves.
//
// Every failure returns ErrInvalidToken wrapped with an operator-facing reason. The
// interceptor logs the reason and sends the client only codes.Unauthenticated: "expired 4
// minutes ago" and "unknown key id" are diagnostic gold for an operator and a free oracle
// for anyone probing the surface.
func (v *OIDCVerifier) Verify(ctx context.Context, raw string) (Principal, error) {
	if strings.TrimSpace(raw) == "" {
		return Principal{}, ErrNoCredentials
	}

	keys, err := v.keySet(ctx)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}

	claims := jwt.MapClaims{}
	_, err = jwt.ParseWithClaims(raw, claims, v.keyFunc(ctx, keys),
		// The allowlist. Without it, jwt.Parse honours whatever the token's header asks
		// for, which is the whole algorithm-confusion class.
		jwt.WithValidMethods(signingAlgorithms),
		jwt.WithIssuer(v.opts.IssuerURL),
		jwt.WithAudience(v.opts.Audience),
		jwt.WithLeeway(v.opts.Leeway),
		// A token with no exp never expires. Requiring it means a leaked token has a
		// bounded blast radius rather than an unbounded one.
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenInvalidAudience) {
			// Called out specifically because it is the failure every Keycloak integration
			// hits first, and the generic message sends people looking in the wrong place.
			v.log.WarnContext(ctx, "token rejected: audience mismatch",
				slog.String("expected_audience", v.opts.Audience),
				slog.String("remedy", "add an Audience protocol mapper to the Keycloak client; a Keycloak access token's default aud is \"account\", not your API"))
		}
		return Principal{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}

	return v.principalFrom(claims)
}

// keyFunc selects the verification key for a token.
//
// The alg binding below is the one check here that catches something no other layer does --
// see the note on signingAlgorithms for which parts of this are load-bearing and which are
// belt and braces.
func (v *OIDCVerifier) keyFunc(ctx context.Context, keys *keyCache) jwt.Keyfunc {
	return func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			// Without a kid there is no way to choose a key. Trying every key in the set
			// gives an attacker N attempts instead of one and is how key-confusion bugs
			// survive rotation.
			return nil, errors.New("token header has no kid")
		}

		pk, err := keys.keyFor(ctx, kid)
		if err != nil {
			return nil, err
		}

		alg, _ := token.Header["alg"].(string)

		// If the JWKS declared an algorithm for this key, it is binding. A key published
		// as RS256 must not verify a PS256 token, even though both use the same RSA key.
		if pk.alg != "" && pk.alg != alg {
			return nil, fmt.Errorf("token alg %q does not match the algorithm %q published for key %q", alg, pk.alg, kid)
		}

		// Independently of the parser's allowlist: the algorithm family must match the key
		// type we actually hold. This is what makes an HS256 token signed with an RSA
		// public key fail even if the allowlist were misconfigured, because the key here is
		// an *rsa.PublicKey and no HMAC method can consume one.
		switch pk.key.(type) {
		case *rsa.PublicKey:
			if !strings.HasPrefix(alg, "RS") && !strings.HasPrefix(alg, "PS") {
				return nil, fmt.Errorf("key %q is RSA but the token asks for %q", kid, alg)
			}
		case *ecdsa.PublicKey:
			if !strings.HasPrefix(alg, "ES") {
				return nil, fmt.Errorf("key %q is ECDSA but the token asks for %q", kid, alg)
			}
		default:
			return nil, fmt.Errorf("key %q has an unsupported type %T", kid, pk.key)
		}

		return pk.key, nil
	}
}

// keySet resolves the JWKS endpoint, retrying a failed discovery on a later request.
func (v *OIDCVerifier) keySet(ctx context.Context) (*keyCache, error) {
	v.mu.RLock()
	cached := v.keys
	v.mu.RUnlock()
	if cached != nil {
		return cached, nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	// Re-check under the write lock: concurrent first requests all queue here, and only one
	// of them should perform discovery.
	if v.keys != nil {
		return v.keys, nil
	}

	// An explicit JWKS URL skips discovery entirely and cannot fail.
	if v.opts.JWKSURL != "" {
		v.keys = newKeyCache(v.opts.JWKSURL, v.client, v.opts.MinRefresh, v.opts.MaxKeyAge, v.log)
		return v.keys, nil
	}

	// Rate-limit retries, so an IdP outage does not turn every inbound request into an
	// outbound discovery request aimed at the IdP that is already struggling.
	if v.opts.MinRefresh > 0 && !v.lastDiscovery.IsZero() && time.Since(v.lastDiscovery) < v.opts.MinRefresh {
		return nil, v.discoverErr
	}
	v.lastDiscovery = time.Now()

	// WithoutCancel: one caller's cancellation must not fail the discovery that every other
	// queued caller is waiting on. A client that hangs up mid-request would otherwise take
	// down authentication for whoever happened to be behind it. The http.Client's own
	// Timeout still bounds this, so it cannot hang forever.
	jwksURL, err := v.discover(context.WithoutCancel(ctx))
	if err != nil {
		v.discoverErr = err
		return nil, err
	}

	v.discoverErr = nil
	v.keys = newKeyCache(jwksURL, v.client, v.opts.MinRefresh, v.opts.MaxKeyAge, v.log)
	return v.keys, nil
}

// discoveryDocument is the subset of the OIDC discovery response this service uses.
type discoveryDocument struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

func (v *OIDCVerifier) discover(ctx context.Context) (string, error) {
	endpoint := strings.TrimSuffix(v.opts.IssuerURL, "/") + "/.well-known/openid-configuration"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := v.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("oidc discovery at %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oidc discovery at %s returned %s", endpoint, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes))
	if err != nil {
		return "", err
	}

	var doc discoveryDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("oidc discovery document is not valid JSON: %w", err)
	}

	// OpenID Connect Discovery §4.3 requires this check, and it is not ceremony: without
	// it, anything that can influence the discovery response -- a redirect, a hijacked DNS
	// entry, a misconfigured proxy -- can point this service at a JWKS it controls, and
	// every token it mints then verifies.
	if doc.Issuer != v.opts.IssuerURL {
		return "", fmt.Errorf("oidc discovery issuer mismatch: document says %q, configured issuer is %q", doc.Issuer, v.opts.IssuerURL)
	}
	if doc.JWKSURI == "" {
		return "", errors.New("oidc discovery document has no jwks_uri")
	}
	if err := requireSecureURL(doc.JWKSURI, "jwks_uri"); err != nil {
		return "", err
	}
	return doc.JWKSURI, nil
}

// requireSecureURL rejects plaintext HTTP to anywhere but loopback.
//
// Bearer tokens and signing keys over cleartext are a wiretap away from a full compromise.
// The loopback exemption is what lets the test suite run an in-process httptest issuer and
// what lets a laptop point at a compose-hosted Keycloak, without weakening anything that
// crosses a network.
func requireSecureURL(raw, field string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL: %w", field, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("%s uses http:// with non-loopback host %q: bearer tokens and signing keys must not cross a network in cleartext", field, u.Hostname())
	default:
		return fmt.Errorf("%s has unsupported scheme %q", field, u.Scheme)
	}
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
