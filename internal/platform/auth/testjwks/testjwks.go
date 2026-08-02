// Package testjwks is an in-process OIDC issuer for tests.
//
// It exists so the entire authentication suite runs with no identity provider, no Docker,
// and no network -- the same promise as the rest of the default test tier. That is not only
// about convenience. Half the tests worth writing need a HOSTILE issuer: one that serves a
// JWKS with a symmetric key in it, mints a token signed with "none", rotates a key
// mid-flight, or goes offline between two requests. No real provider will do any of that on
// request, so the alternative to this package is not "test against Keycloak", it is "do not
// test the cases that matter".
//
// Everything here is deliberately permissive; it is an attacker's toolkit as much as an
// issuer. Never import it outside a test.
package testjwks

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Issuer is a running OIDC issuer backed by an httptest server.
type Issuer struct {
	t   *testing.T
	srv *httptest.Server

	mu     sync.RWMutex
	ecKey  *ecdsa.PrivateKey
	ecKid  string
	rsaKey *rsa.PrivateKey
	rsaKid string

	// extraKeys are published in the JWKS but not used for signing, so a test can put a
	// symmetric key or a short RSA key in front of the parser.
	extraKeys []json.RawMessage

	// jwksFetches counts JWKS requests. Caching is otherwise unobservable from outside,
	// and "we cache the key set" is exactly the kind of claim that quietly stops being
	// true.
	jwksFetches atomic.Int64

	// offline makes every endpoint return 503 without tearing down the listener.
	offline atomic.Bool

	// jwksDelay stalls the JWKS response, standing in for a slow or blackholed IdP.
	jwksDelay atomic.Int64

	// discoveryIssuer overrides the `issuer` field of the discovery document, so a test can
	// serve a document that disagrees with the URL it was fetched from.
	discoveryIssuer atomic.Pointer[string]

	// Audience and Tenant seed DefaultClaims.
	Audience    string
	TenantClaim string
	ScopeClaim  string
}

// New starts an issuer and registers its shutdown with t.Cleanup.
func New(t *testing.T) *Issuer {
	t.Helper()

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ec key: %v", err)
	}
	// 2048 bits rather than 1024: the verifier refuses undersized moduli, so a smaller key
	// here would fail every test for a reason unrelated to what it is testing.
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	i := &Issuer{
		t:           t,
		ecKey:       ecKey,
		ecKid:       "ec-key-1",
		rsaKey:      rsaKey,
		rsaKid:      "rsa-key-1",
		Audience:    "orderd",
		TenantClaim: "tenant_id",
		ScopeClaim:  "scope",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", i.guard(i.handleDiscovery))
	mux.HandleFunc("/jwks.json", i.guard(i.handleJWKS))
	i.srv = httptest.NewServer(mux)

	t.Cleanup(i.srv.Close)
	return i
}

// URL is the issuer identifier, and the value the `iss` claim carries.
func (i *Issuer) URL() string { return i.srv.URL }

// JWKSURL is the key set endpoint.
func (i *Issuer) JWKSURL() string { return i.srv.URL + "/jwks.json" }

// Client is an http.Client wired to this issuer.
func (i *Issuer) Client() *http.Client { return i.srv.Client() }

// JWKSFetches reports how many times the key set has been fetched.
func (i *Issuer) JWKSFetches() int { return int(i.jwksFetches.Load()) }

// Close stops serving permanently. Use it for an IdP that is gone; t.Cleanup calling it a
// second time is harmless.
func (i *Issuer) Close() { i.srv.Close() }

// SetOffline toggles a RECOVERABLE outage: every endpoint answers 503 until it is turned
// back on, at the same URL.
//
// Close cannot express this, because a closed httptest server cannot reopen on its port --
// and "comes back at the same address" is exactly the scenario that matters. A verifier that
// memoises a discovery failure permanently looks fine against Close (the IdP never returns)
// and is broken forever against this.
func (i *Issuer) SetOffline(offline bool) { i.offline.Store(offline) }

// guard makes every handler respect the offline switch.
func (i *Issuer) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if i.offline.Load() {
			http.Error(w, "identity provider is restarting", http.StatusServiceUnavailable)
			return
		}
		next(w, r)
	}
}

// Rotate replaces the EC signing key with a fresh one under a new key id.
//
// Tokens already minted stay signed by the OLD key, which is the realistic shape of a
// rotation: a verifier holding a cached key set must refetch to learn the new key, and
// must not have thrown away the old one while tokens signed by it are still valid.
func (i *Issuer) Rotate(newKid string) {
	i.t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		i.t.Fatalf("rotate: %v", err)
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	i.ecKey = key
	i.ecKid = newKid
}

// PublishRawKey adds an arbitrary JSON object to the published key set.
//
// This is how a test puts a key the verifier must REFUSE in front of it -- a symmetric
// "oct" key, a 1024-bit RSA key, an unknown key type. Those refusals live in jwks.go and are
// worthless untested.
func (i *Issuer) PublishRawKey(raw string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.extraKeys = append(i.extraKeys, json.RawMessage(raw))
}

// DefaultClaims is a valid, unexpired token body for this issuer.
//
// Tests mutate the returned map to build the case they need -- delete a claim, push exp into
// the past, change aud -- which keeps each test's deviation from valid visible on one line.
func (i *Issuer) DefaultClaims() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss":         i.URL(),
		"aud":         i.Audience,
		"sub":         "user-123",
		"exp":         now.Add(time.Hour).Unix(),
		"iat":         now.Unix(),
		"nbf":         now.Add(-time.Minute).Unix(),
		i.TenantClaim: "tenant-a",
		i.ScopeClaim:  "orders:read orders:write",
	}
}

// Sign mints a token signed with the current EC key (ES256), the normal path.
func (i *Issuer) Sign(claims jwt.MapClaims) string {
	i.t.Helper()

	i.mu.RLock()
	key, kid := i.ecKey, i.ecKid
	i.mu.RUnlock()

	return i.sign(jwt.SigningMethodES256, key, kid, claims)
}

// SignRSA mints a token signed with the RSA key (RS256).
func (i *Issuer) SignRSA(claims jwt.MapClaims) string {
	i.t.Helper()

	i.mu.RLock()
	key, kid := i.rsaKey, i.rsaKid
	i.mu.RUnlock()

	return i.sign(jwt.SigningMethodRS256, key, kid, claims)
}

// SignPS256 signs with the RSA key using PS256, while the published JWKS entry for that key
// declares alg RS256.
//
// Both algorithms use the same RSA key, so the signature verifies. The only thing wrong is
// that the issuer said this key is for RS256 and the token used it for something else. It is
// the one algorithm-substitution case a verifier can actually catch by itself, which makes
// it the honest test of the alg-binding check in oidc.go's keyFunc.
func (i *Issuer) SignPS256(claims jwt.MapClaims) string {
	i.t.Helper()

	i.mu.RLock()
	key, kid := i.rsaKey, i.rsaKid
	i.mu.RUnlock()

	return i.sign(jwt.SigningMethodPS256, key, kid, claims)
}

// SignWithKid mints a token whose header names a key id that may not exist.
func (i *Issuer) SignWithKid(kid string, claims jwt.MapClaims) string {
	i.t.Helper()

	i.mu.RLock()
	key := i.ecKey
	i.mu.RUnlock()

	return i.sign(jwt.SigningMethodES256, key, kid, claims)
}

// SignNone mints an UNSIGNED token with alg "none".
//
// The original JWT vulnerability, and still worth a test: a verifier that reads the
// algorithm from the token and dispatches on it accepts a token anyone can write.
func (i *Issuer) SignNone(claims jwt.MapClaims) string {
	i.t.Helper()

	i.mu.RLock()
	kid := i.ecKid
	i.mu.RUnlock()

	// golang-jwt requires this sentinel to sign with "none", which is itself good design:
	// you cannot do it by accident.
	return i.sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, kid, claims)
}

// SignHS256WithRSAPublicKey mints the ALGORITHM CONFUSION attack.
//
// The attacker cannot sign with the issuer's private key, so they do not try. They take the
// PUBLIC key -- published in the JWKS, readable by anyone -- and use its bytes as an HMAC
// secret, setting alg to HS256. A verifier that picks its algorithm from the token header
// then "verifies" the token against the same public bytes, and succeeds.
//
// This is the single most important token in this file. If the verifier ever accepts it,
// authentication is decorative.
func (i *Issuer) SignHS256WithRSAPublicKey(claims jwt.MapClaims) string {
	i.t.Helper()

	i.mu.RLock()
	pub, kid := &i.rsaKey.PublicKey, i.rsaKid
	i.mu.RUnlock()

	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		i.t.Fatalf("marshal public key: %v", err)
	}
	secret := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})

	return i.sign(jwt.SigningMethodHS256, secret, kid, claims)
}

// SignHMAC mints a token signed with a symmetric secret under the given key id.
//
// Paired with PublishSymmetricKey, this is the attack the "oct keys are refused" rule in
// jwks.go exists to stop: an issuer that publishes a symmetric key has published the ability
// to mint tokens to everyone who can read the JWKS.
func (i *Issuer) SignHMAC(kid string, secret []byte, claims jwt.MapClaims) string {
	i.t.Helper()
	return i.sign(jwt.SigningMethodHS256, secret, kid, claims)
}

// PublishSymmetricKey adds an "oct" entry to the JWKS, carrying the secret in the clear --
// which is what an "oct" entry in a public key set always does.
func (i *Issuer) PublishSymmetricKey(kid string, secret []byte) {
	i.t.Helper()
	i.PublishRawKey(string(mustJSON(i.t, map[string]any{
		"kty": "oct",
		"kid": kid,
		"alg": "HS256",
		"use": "sig",
		"k":   base64.RawURLEncoding.EncodeToString(secret),
	})))
}

// PublishWeakRSAKey adds a 1024-bit RSA key, which the verifier must refuse as undersized.
func (i *Issuer) PublishWeakRSAKey(kid string) *rsa.PrivateKey {
	i.t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		i.t.Fatalf("generate weak rsa key: %v", err)
	}
	i.PublishRawKey(string(mustJSON(i.t, rsaJWK(key, kid))))
	return key
}

// SignRSAWith signs with a caller-supplied RSA key, for keys this issuer published but does
// not otherwise use.
func (i *Issuer) SignRSAWith(key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	i.t.Helper()
	return i.sign(jwt.SigningMethodRS256, key, kid, claims)
}

func (i *Issuer) sign(method jwt.SigningMethod, key any, kid string, claims jwt.MapClaims) string {
	i.t.Helper()

	token := jwt.NewWithClaims(method, claims)
	token.Header["kid"] = kid

	signed, err := token.SignedString(key)
	if err != nil {
		i.t.Fatalf("sign token: %v", err)
	}
	return signed
}

// SetDiscoveryIssuer makes the discovery document claim a different issuer than the one it
// was fetched from.
//
// This is the ONLY way to exercise the OpenID Connect Discovery 4.3 issuer-match check. The
// obvious alternative -- pointing the verifier at a URL this issuer does not serve -- fails
// on a 404 long before the issuer comparison happens, so the test passes while never
// reaching the code it names.
func (i *Issuer) SetDiscoveryIssuer(issuer string) { i.discoveryIssuer.Store(&issuer) }

func (i *Issuer) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	issuer := i.URL()
	if override := i.discoveryIssuer.Load(); override != nil {
		issuer = *override
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":   issuer,
		"jwks_uri": i.JWKSURL(),
	})
}

// SetJWKSDelay makes the key-set endpoint take d to respond.
//
// A slow IdP is the interesting failure, not a dead one: it is what turns a lock held across
// the fetch into a stall of every concurrent verification.
func (i *Issuer) SetJWKSDelay(d time.Duration) { i.jwksDelay.Store(int64(d)) }

func (i *Issuer) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	i.jwksFetches.Add(1)
	if d := time.Duration(i.jwksDelay.Load()); d > 0 {
		time.Sleep(d)
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	keys := []json.RawMessage{
		mustJSON(i.t, ecJWK(i.ecKey, i.ecKid)),
		mustJSON(i.t, rsaJWK(i.rsaKey, i.rsaKid)),
	}
	keys = append(keys, i.extraKeys...)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
}

func ecJWK(key *ecdsa.PrivateKey, kid string) map[string]any {
	// RFC 7518 §6.2.1.2: coordinates are fixed-width, left-padded to the field size.
	// FillBytes rather than Bytes, because a coordinate with leading zero bytes would
	// otherwise be published one or two bytes short -- which real issuers get wrong often
	// enough that the verifier checks the length.
	const size = 32
	return map[string]any{
		"kty": "EC",
		"crv": "P-256",
		"kid": kid,
		"alg": "ES256",
		"use": "sig",
		"x":   base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, size))),
		"y":   base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, size))),
	}
}

func rsaJWK(key *rsa.PrivateKey, kid string) map[string]any {
	return map[string]any{
		"kty": "RSA",
		"kid": kid,
		"alg": "RS256",
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal jwk: %v", err)
	}
	return b
}
