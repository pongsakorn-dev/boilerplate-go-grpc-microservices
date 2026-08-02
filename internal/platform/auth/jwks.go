package auth

import (
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Why this file exists rather than a JWKS library.
//
// Signature verification is NOT hand-rolled -- golang-jwt/jwt does that, and nobody should
// write their own ECDSA verify. What lives here is key TRANSPORT: fetch a JSON document,
// cache it, parse public keys out of it, and decide when to refetch. That is ordinary code,
// and owning it buys three things the alternatives did not:
//
//   - Zero transitive dependencies. golang-jwt/jwt/v5 is pure stdlib, so the whole auth
//     stack costs exactly one module. coreos/go-oidc costs three, one of which
//     (golang.org/x/oauth2) is entirely unused by a server that only verifies.
//   - Testable cache semantics. The suite asserts a fetch COUNT to prove caching, drives a
//     key rotation, and pulls the issuer offline mid-flight. Those assertions are direct
//     against code we own and awkward against a library's private cache.
//   - The refusals below are visible. A JWKS containing a symmetric key, or an undersized
//     RSA modulus, is rejected here in code a reader can see, rather than in a dependency
//     they must take on trust.

// keyCache fetches and caches an issuer's public keys.
//
// Safe for concurrent use. Refresh is serialised: a rotation that invalidates every
// in-flight request must not turn into one JWKS fetch per request.
type keyCache struct {
	url    string
	client *http.Client
	log    *slog.Logger

	// minRefresh rate-limits rotation-triggered refetches. Without it, a token signed by a
	// key this service will never know about -- a forgery, or traffic from a decommissioned
	// issuer -- becomes an unbounded HTTP request amplifier pointed at the IdP.
	minRefresh time.Duration

	// maxAge bounds how long a cached key set is served without revalidation.
	//
	// WITHOUT THIS, KEY REVOCATION NEVER TAKES EFFECT. Refetching only on a cache MISS
	// sounds sufficient and is not: when an issuer deletes a compromised key it keeps
	// signing with its other, still-valid keys -- the normal shape at Keycloak, Google and
	// Auth0, which all publish several at once. Every legitimate token then names a kid this
	// cache already holds, so a miss never happens, so no refetch ever happens, and the
	// deleted key stays trusted until the pod restarts. Anyone holding a token signed by the
	// revoked key keeps authenticating indefinitely.
	maxAge time.Duration

	// refreshMu serialises FETCHES. It is separate from mu on purpose.
	//
	// Holding the map's write lock across the HTTP request is the obvious implementation and
	// it hands an unauthenticated attacker a stall: keyFunc runs before signature
	// verification, so one garbage token naming an unknown kid triggers a refetch, and every
	// concurrent request -- including ones whose key is already cached and would have
	// succeeded instantly -- blocks on the mutex for as long as the IdP takes to answer, up
	// to the client timeout. sync.RWMutex is not context-aware, so those requests ignore
	// their own deadlines, and because auth sits inside the admission slot they consume the
	// whole admission budget while they wait.
	//
	// So: refreshMu serialises the network call, mu is held only for the microseconds needed
	// to read or swap the map, and no reader ever waits on I/O.
	refreshMu sync.Mutex

	mu          sync.RWMutex
	keys        map[string]parsedKey
	fetchedAt   time.Time
	lastAttempt time.Time
	warm        bool
}

// parsedKey is one verified-usable public key.
type parsedKey struct {
	key crypto.PublicKey

	// alg is the algorithm the JWKS declared for this key, if any. When present it is
	// enforced: a key published as RS256 may not be used to verify an ES256 token even if
	// the key type would technically permit it.
	alg string
}

const (
	// maxJWKSBytes bounds the response body. An endpoint that returns gigabytes -- through
	// compromise, misconfiguration, or a proxy serving the wrong thing -- must not be able
	// to exhaust this process's memory.
	maxJWKSBytes = 1 << 20

	// minRSABits refuses undersized RSA keys. 1024-bit RSA is factorable by well-resourced
	// attackers; accepting it because an IdP still publishes it defeats the point.
	minRSABits = 2048

	// maxRSABits refuses ABSURDLY LARGE keys, which is the less obvious half.
	//
	// RSA verification cost grows with the modulus, so a JWKS entry carrying a 500,000-bit
	// modulus makes every signature check against it cost enormous, uninterruptible CPU
	// inside crypto/rsa -- which no context deadline can cancel, because the maths does not
	// check for cancellation. 8192 bits is far above anything a real issuer publishes.
	maxRSABits = 8192

	// defaultMaxAge is how long a key set is served before revalidation. See keyCache.maxAge:
	// without an upper bound, a revoked key is trusted until the process restarts.
	defaultMaxAge = 15 * time.Minute
)

func newKeyCache(url string, client *http.Client, minRefresh, maxAge time.Duration, log *slog.Logger) *keyCache {
	if maxAge == 0 {
		maxAge = defaultMaxAge
	}
	return &keyCache{
		url:        url,
		client:     client,
		log:        log,
		minRefresh: minRefresh,
		maxAge:     maxAge,
		keys:       make(map[string]parsedKey),
	}
}

// keyFor returns the public key with the given id, refetching once if it is unknown.
//
// FAIL-CLOSED AND ITS LIMITS. Three distinct situations get three different answers, and
// conflating them is the usual mistake:
//
//	no cached keys + fetch fails        -> error. Nothing can be verified, so nothing is.
//	cached keys + fetch fails + kid hit -> ACCEPT. The IdP being unreachable does not
//	                                      invalidate a signature made by a key we hold.
//	                                      Refusing here converts an IdP blip into a total
//	                                      outage of every service that trusts it.
//	cached keys + fetch fails + kid miss-> error. An unknown key is an unknown key.
//
// The middle case is the one people get wrong in both directions: some fail closed and take
// themselves down with the IdP, others cache forever and keep honouring a revoked key. Both
// halves are handled: a cache hit is served through an outage, AND a hit older than maxAge is
// revalidated first, so revocation actually takes effect.
func (c *keyCache) keyFor(ctx context.Context, kid string) (parsedKey, error) {
	if k, fresh, ok := c.lookup(kid); ok && fresh {
		return k, nil
	}

	refreshErr := c.refresh(ctx)

	// Look again: a successful refresh may have brought the key in, and a STALE hit is still
	// a hit if the refresh failed -- serving a key we already hold through an IdP outage is
	// the deliberate behaviour, and it must not regress just because the key aged out.
	if k, _, ok := c.lookup(kid); ok {
		return k, nil
	}

	if refreshErr != nil {
		c.mu.RLock()
		warm := c.warm
		c.mu.RUnlock()
		if !warm {
			return parsedKey{}, fmt.Errorf("jwks unavailable and no keys cached: %w", refreshErr)
		}
		// Warm cache, failed refresh, and the kid was not in it. The miss leads, because
		// that is the actionable fact; the fetch error is wrapped rather than formatted so
		// the chain survives for an operator using errors.As.
		return parsedKey{}, fmt.Errorf("unknown key id %q (refresh failed: %w)", kid, refreshErr)
	}
	return parsedKey{}, fmt.Errorf("unknown key id %q", kid)
}

// lookup reports the key, whether the cache is within maxAge, and whether the key was found.
func (c *keyCache) lookup(kid string) (parsedKey, bool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	k, ok := c.keys[kid]
	fresh := c.maxAge <= 0 || time.Since(c.fetchedAt) < c.maxAge
	return k, fresh, ok
}

// refresh fetches the key set, at most once per minRefresh window.
//
// THE NETWORK CALL HAPPENS WITH NO READER LOCK HELD. refreshMu serialises fetches so a
// thundering herd produces one request; c.mu is taken only to read state and to swap the map.
// Holding c.mu across the fetch -- the obvious version -- lets one unauthenticated request
// naming an unknown kid stall every concurrent verification for the IdP's full response time,
// including ones whose key is already cached.
func (c *keyCache) refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	// Re-check after acquiring: several goroutines miss the same kid and queue here, and
	// only the first should hit the network. The rest find the window closed and return.
	//
	// minRefresh <= 0 disables the limit; see OIDCOptions.MinRefresh for why that escape
	// hatch exists rather than a very small positive value.
	c.mu.RLock()
	lastAttempt, warm := c.lastAttempt, c.warm
	c.mu.RUnlock()

	if c.minRefresh > 0 && !lastAttempt.IsZero() && time.Since(lastAttempt) < c.minRefresh {
		if warm {
			return nil
		}
		return errors.New("jwks refresh rate-limited and no keys cached")
	}

	c.mu.Lock()
	c.lastAttempt = time.Now()
	c.mu.Unlock()

	// WithoutCancel for the same reason as discovery: this fetch is SHARED. Other goroutines
	// are serialised behind refreshMu waiting for its result, so letting the first caller's
	// cancellation abort it means one client hanging up denies authentication to everyone
	// behind them. http.Client.Timeout bounds it instead.
	keys, err := c.fetch(context.WithoutCancel(ctx))
	if err != nil {
		c.log.WarnContext(ctx, "jwks refresh failed",
			slog.String("url", c.url), slog.String("error", err.Error()), slog.Bool("cache_warm", warm))
		return err
	}
	if len(keys) == 0 {
		return errors.New("jwks contained no usable signing keys")
	}

	c.mu.Lock()
	c.keys = keys
	c.fetchedAt = time.Now()
	c.warm = true
	c.mu.Unlock()

	return nil
}

func (c *keyCache) fetch(ctx context.Context) (map[string]parsedKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks endpoint returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes))
	if err != nil {
		return nil, err
	}

	var doc jwksDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("jwks is not valid JSON: %w", err)
	}

	out := make(map[string]parsedKey, len(doc.Keys))
	for _, jwk := range doc.Keys {
		key, err := jwk.parse()
		if err != nil {
			// One bad key must not discard the good ones. An IdP publishing a key type we
			// do not support (or must not support) is normal; refusing the whole document
			// would take authentication down for a reason unrelated to the caller.
			c.log.WarnContext(ctx, "skipping unusable jwks entry",
				slog.String("kid", jwk.Kid), slog.String("kty", jwk.Kty), slog.String("reason", err.Error()))
			continue
		}
		if jwk.Kid == "" {
			// A key with no id cannot be selected by a token's `kid` header. Rather than
			// guessing -- "try every key" is how algorithm-confusion bugs get a second
			// chance -- require the issuer to identify its keys.
			c.log.WarnContext(ctx, "skipping jwks entry with no key id", slog.String("kty", jwk.Kty))
			continue
		}
		out[jwk.Kid] = parsedKey{key: key, alg: jwk.Alg}
	}
	return out, nil
}

type jwksDocument struct {
	Keys []jwk `json:"keys"`
}

// jwk is one JSON Web Key (RFC 7517). Only the fields needed to build a public key are
// modelled; unknown fields are ignored by encoding/json.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`

	// RSA
	N string `json:"n"`
	E string `json:"e"`

	// EC
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// parse converts a JWK into a usable public key, or explains why it will not.
func (j jwk) parse() (crypto.PublicKey, error) {
	// `use` is optional, but when the issuer says a key is for encryption, believe it.
	if j.Use != "" && j.Use != "sig" {
		return nil, fmt.Errorf("key use %q is not %q", j.Use, "sig")
	}

	switch j.Kty {
	case "RSA":
		return j.parseRSA()
	case "EC":
		return j.parseEC()

	case "oct":
		// THE IMPORTANT REFUSAL.
		//
		// `oct` is a symmetric key, and its value is published in the JWKS -- which for a
		// public JWKS endpoint means published to everyone. A verifier that loads it and
		// accepts HMAC algorithms lets any reader of the JWKS mint valid tokens. This is
		// the same family as the alg:none and HS256-signed-with-the-RSA-public-key attacks,
		// and the defence is the same: never let a symmetric key into the verification
		// path. oidc_verifier_test.go proves all three are rejected.
		return nil, errors.New("symmetric (oct) keys are refused: a published symmetric key lets anyone who can read the JWKS forge tokens")

	default:
		return nil, fmt.Errorf("unsupported key type %q", j.Kty)
	}
}

func (j jwk) parseRSA() (crypto.PublicKey, error) {
	nBytes, err := b64(j.N)
	if err != nil {
		return nil, fmt.Errorf("modulus: %w", err)
	}
	eBytes, err := b64(j.E)
	if err != nil {
		return nil, fmt.Errorf("exponent: %w", err)
	}
	if len(eBytes) == 0 || len(eBytes) > 8 {
		return nil, fmt.Errorf("exponent is %d bytes, want 1-8", len(eBytes))
	}

	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() || e.Int64() > int64(^uint32(0)) {
		return nil, errors.New("exponent does not fit in an int")
	}

	key := &rsa.PublicKey{N: n, E: int(e.Int64())}
	if key.N.Sign() <= 0 {
		return nil, errors.New("modulus is not positive")
	}
	if key.E < 3 || key.E%2 == 0 {
		return nil, fmt.Errorf("exponent %d is invalid", key.E)
	}
	// Bounded at BOTH ends. Undersized is the obvious risk; oversized is a CPU exhaustion
	// vector, because rsa.VerifyPKCS1v15 on a gigantic modulus runs for a very long time
	// inside code that never checks for cancellation, so no deadline can stop it.
	if bits := key.N.BitLen(); bits < minRSABits || bits > maxRSABits {
		return nil, fmt.Errorf("modulus is %d bits, want between %d and %d", bits, minRSABits, maxRSABits)
	}
	return key, nil
}

func (j jwk) parseEC() (crypto.PublicKey, error) {
	var curve elliptic.Curve
	var ecdhCurve ecdh.Curve
	var size int

	switch j.Crv {
	case "P-256":
		curve, ecdhCurve, size = elliptic.P256(), ecdh.P256(), 32
	case "P-384":
		curve, ecdhCurve, size = elliptic.P384(), ecdh.P384(), 48
	case "P-521":
		// 521 bits is 65.125 bytes, which RFC 7518 rounds up to 66.
		curve, ecdhCurve, size = elliptic.P521(), ecdh.P521(), 66
	default:
		return nil, fmt.Errorf("unsupported curve %q", j.Crv)
	}

	xBytes, err := b64(j.X)
	if err != nil {
		return nil, fmt.Errorf("x: %w", err)
	}
	yBytes, err := b64(j.Y)
	if err != nil {
		return nil, fmt.Errorf("y: %w", err)
	}
	// RFC 7518 §6.2.1.2 requires the octet strings be exactly the field size, left-padded.
	// A short value is not "the same number with fewer bytes" here -- it changes where the
	// coordinate sits in the fixed-width point encoding below.
	if len(xBytes) != size || len(yBytes) != size {
		return nil, fmt.Errorf("coordinates are %d/%d bytes, want %d each for %s", len(xBytes), len(yBytes), size, j.Crv)
	}

	// Validate the point is actually on the curve by round-tripping through crypto/ecdh,
	// whose NewPublicKey does full validation. elliptic.Curve.IsOnCurve is deprecated, and
	// building an ecdsa.PublicKey from unchecked coordinates is how you get a verify call
	// against a point that is not in the group.
	point := make([]byte, 0, 1+2*size)
	point = append(point, 0x04) // uncompressed
	point = append(point, xBytes...)
	point = append(point, yBytes...)
	if _, err := ecdhCurve.NewPublicKey(point); err != nil {
		return nil, fmt.Errorf("point is not on curve %s: %w", j.Crv, err)
	}

	return &ecdsa.PublicKey{
		Curve: curve,
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}, nil
}

// b64 decodes a JWK parameter.
//
// RFC 7515 §2 specifies base64url with padding removed. Some issuers pad anyway, so the
// padding is trimmed rather than rejected -- being strict here breaks interoperability
// without buying any security, since the decoded bytes are identical either way.
func b64(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("empty")
	}
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}
