package auth_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/example/gomicro/internal/platform/auth"
	"github.com/example/gomicro/internal/platform/auth/testjwks"
)

// TestKeySetIsFetchedOnceAndReused turns "we cache the key set" from a comment into a fact.
//
// Caching is invisible from outside -- a verifier that refetched the JWKS on every single
// request would pass every other test in this package while adding an HTTP round trip to
// each RPC and a hard dependency on the IdP being up. The fetch counter is the only way to
// see it, which is why testjwks has one.
func TestKeySetIsFetchedOnceAndReused(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	v := newVerifier(t, iss)

	for range 20 {
		if _, err := v.Verify(context.Background(), iss.Sign(iss.DefaultClaims())); err != nil {
			t.Fatalf("verify: %v", err)
		}
	}

	// One discovery fetch's worth of key fetching, not twenty.
	if got := iss.JWKSFetches(); got != 1 {
		t.Errorf("JWKS was fetched %d times for 20 verifications, want 1.\n\n"+
			"Every extra fetch is an HTTP round trip on the request path and one more way for "+
			"an IdP blip to become an outage here.", got)
	}
}

// TestRotationIsPickedUpWithoutRestart is the operational property that matters most.
//
// Issuers rotate keys on a schedule, and nobody restarts every consumer when they do. A
// verifier that caches forever starts rejecting every token the moment its issuer rotates --
// a total outage, arriving on the IdP's timetable rather than yours.
func TestRotationIsPickedUpWithoutRestart(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	v := newVerifier(t, iss, func(o *auth.OIDCOptions) {
		// No refresh rate limit for this test: the rotation and the retry happen
		// microseconds apart, which the production default would (correctly) suppress.
		o.MinRefresh = -1
	})

	if _, err := v.Verify(context.Background(), iss.Sign(iss.DefaultClaims())); err != nil {
		t.Fatalf("before rotation: %v", err)
	}

	iss.Rotate("ec-key-2")

	if _, err := v.Verify(context.Background(), iss.Sign(iss.DefaultClaims())); err != nil {
		t.Fatalf("after rotation the verifier still rejects valid tokens: %v\n\n"+
			"An unknown key id must trigger a refetch. Without that, every issuer key rotation "+
			"is a full outage for this service.", err)
	}

	if iss.JWKSFetches() < 2 {
		t.Errorf("JWKS fetched %d times; a rotation must cause a refetch", iss.JWKSFetches())
	}
}

// TestUnreachableIssuerFailsClosed: no keys, no verification. Nothing subtle, but it is the
// half of the fail-closed rule people get right, so it anchors the half below.
func TestUnreachableIssuerFailsClosed(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	token := iss.Sign(iss.DefaultClaims())
	v := newVerifier(t, iss)

	// Down before the verifier ever warmed its cache.
	iss.Close()

	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("a token was accepted while the issuer was unreachable and no keys were cached")
	}
}

// TestWarmCacheSurvivesAnIssuerOutage is the half people get wrong, in both directions.
//
// A verifier that refuses everything the moment its IdP hiccups converts a dependency blip
// into a total outage of every service that trusts that IdP -- and IdPs are shared, so that
// is an estate-wide outage. The signature on a token minted five minutes ago is not less
// valid because the issuer's web server is restarting.
//
// The opposite error is caching forever and honouring a revoked key indefinitely. The
// balance struck in jwks.go: keys already held stay usable, and any UNKNOWN key id still
// forces a refetch, so an outage cannot smuggle in a key we never saw.
func TestWarmCacheSurvivesAnIssuerOutage(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	v := newVerifier(t, iss)

	// Warm the cache.
	if _, err := v.Verify(context.Background(), iss.Sign(iss.DefaultClaims())); err != nil {
		t.Fatalf("warming: %v", err)
	}

	// Mint a token BEFORE the outage, then take the issuer down.
	token := iss.Sign(iss.DefaultClaims())
	iss.Close()

	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Errorf("a valid token signed by a CACHED key was rejected during an issuer outage: %v\n\n"+
			"This turns an IdP restart into a total outage for every service that trusts it. "+
			"The signature is still valid; the issuer being unreachable does not change that.", err)
	}
}

// TestUnknownKeyDuringAnOutageIsStillRefused is the boundary of the previous test.
//
// Serving cached keys through an outage must not become "accept anything during an outage".
func TestUnknownKeyDuringAnOutageIsStillRefused(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	// No refresh rate limit; see OIDCOptions.MinRefresh for why -1 rather than a tiny value.
	v := newVerifier(t, iss, func(o *auth.OIDCOptions) { o.MinRefresh = -1 })

	if _, err := v.Verify(context.Background(), iss.Sign(iss.DefaultClaims())); err != nil {
		t.Fatalf("warming: %v", err)
	}

	forged := iss.SignWithKid("key-we-have-never-seen", iss.DefaultClaims())
	iss.Close()

	if _, err := v.Verify(context.Background(), forged); err == nil {
		t.Fatal("accepted an unknown key id while the issuer was unreachable -- an outage " +
			"must not become an opportunity to introduce a key")
	}
}

// TestRefreshIsRateLimited stops an unknown key id becoming an HTTP amplifier.
//
// Every miss triggers a refetch, so a flood of tokens signed by a key that will never exist
// -- forgeries, or traffic from a decommissioned issuer -- becomes one JWKS request per
// request, pointed at the IdP. That is an outbound DoS launched from your own service, on
// behalf of whoever is attacking you.
func TestRefreshIsRateLimited(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	v := newVerifier(t, iss, func(o *auth.OIDCOptions) { o.MinRefresh = time.Hour })

	if _, err := v.Verify(context.Background(), iss.Sign(iss.DefaultClaims())); err != nil {
		t.Fatalf("warming: %v", err)
	}
	afterWarm := iss.JWKSFetches()

	for range 50 {
		_, _ = v.Verify(context.Background(), iss.SignWithKid("never-exists", iss.DefaultClaims()))
	}

	if extra := iss.JWKSFetches() - afterWarm; extra > 1 {
		t.Errorf("50 unknown-kid tokens caused %d JWKS fetches, want at most 1 within the refresh window.\n\n"+
			"Unbounded refetching lets an attacker aim your service at your identity provider.", extra)
	}
}

// TestUnusableKeysAreRefused covers the two entries jwks.go declines to load, and both
// refusals are security decisions rather than compatibility ones.
func TestUnusableKeysAreRefused(t *testing.T) {
	t.Parallel()

	t.Run("symmetric oct key", func(t *testing.T) {
		t.Parallel()

		// A symmetric key in a PUBLIC key set publishes the ability to mint tokens to
		// everyone who can read it. This is the same family as algorithm confusion, arriving
		// through the front door instead: the issuer simply hands out the signing secret.
		//
		// WHICH LAYER STOPS IT, stated honestly: three do. The oct refusal in jwk.parse, the
		// HS* absence from the signing allowlist, and keyFunc's key-type switch. Removing any
		// ONE leaves this test passing; removing the parse refusal and the type switch
		// together makes it fail (verified by doing exactly that). So it is a behavioural
		// assertion with real teeth, not a unit test of the oct branch -- and the layer this
		// subtest is filed under is not the only thing holding the line.
		iss := testjwks.New(t)
		secret := []byte("this-secret-is-published-in-the-jwks")
		iss.PublishSymmetricKey("oct-key", secret)

		v := newVerifier(t, iss)

		token := iss.SignHMAC("oct-key", secret, iss.DefaultClaims())
		if _, err := v.Verify(context.Background(), token); err == nil {
			t.Fatal("accepted a token signed with a symmetric key published in the JWKS -- " +
				"anyone who can fetch that key set can now mint valid credentials")
		}
	})

	t.Run("undersized RSA key", func(t *testing.T) {
		t.Parallel()

		// 1024-bit RSA is within reach of well-resourced attackers. Accepting it because an
		// IdP still publishes it defeats the purpose of checking signatures at all.
		iss := testjwks.New(t)
		weak := iss.PublishWeakRSAKey("weak-rsa")

		v := newVerifier(t, iss)

		token := iss.SignRSAWith(weak, "weak-rsa", iss.DefaultClaims())
		if _, err := v.Verify(context.Background(), token); err == nil {
			t.Fatal("accepted a token signed by a 1024-bit RSA key")
		}
	})

	t.Run("one bad key does not poison the good ones", func(t *testing.T) {
		t.Parallel()

		// A refusal must be per-key. Rejecting the whole document because the issuer
		// published one key type we decline would take authentication down for a reason
		// entirely unrelated to the caller -- and IdPs publish extra keys routinely.
		iss := testjwks.New(t)
		iss.PublishSymmetricKey("oct-key", []byte("irrelevant"))
		iss.PublishRawKey(`{"kty":"OKP","crv":"Ed25519","kid":"unsupported","x":"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"}`)

		v := newVerifier(t, iss)

		if _, err := v.Verify(context.Background(), iss.Sign(iss.DefaultClaims())); err != nil {
			t.Errorf("a valid token was rejected because the key set also contained keys we decline: %v", err)
		}
	})
}

// TestDiscoveryRecoversAfterAFailedFirstAttempt guards an operational trap the first
// implementation walked straight into.
//
// Discovery was memoised with sync.Once, so a pod whose very first request happened to
// arrive while the IdP was restarting cached that error for the life of the process and
// never authenticated anyone again. Nothing recovered it: the liveness probe reports process
// health by design, so Kubernetes never restarted the pod. It served Unauthenticated
// forever, and every dashboard said it was fine.
func TestDiscoveryRecoversAfterAFailedFirstAttempt(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	// Retry on the next request rather than waiting out the refresh window.
	v := newVerifier(t, iss, func(o *auth.OIDCOptions) { o.MinRefresh = -1 })

	token := iss.Sign(iss.DefaultClaims())

	// The IdP is restarting when this pod's very first request arrives.
	iss.SetOffline(true)
	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("verification succeeded while discovery was failing")
	}

	// It comes back, at the same address.
	iss.SetOffline(false)

	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatalf("the verifier never recovered after the identity provider came back: %v\n\n"+
			"A discovery failure must not be cached for the life of the process. If it is, this "+
			"pod serves Unauthenticated forever -- and nothing restarts it, because the liveness "+
			"probe reports process health by design.", err)
	}
}

// TestDiscoveryIssuerMismatchIsRefused covers OpenID Connect Discovery §4.3.
//
// Without the check, anything that can influence the discovery response -- a redirect, a
// hijacked DNS record, a misconfigured corporate proxy -- points this service at a JWKS the
// attacker controls, and every token they mint then verifies perfectly.
func TestDiscoveryIssuerMismatchIsRefused(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)

	// The verifier is configured with the issuer's REAL url, so discovery succeeds and
	// returns 200 -- the document itself then claims to be somebody else.
	//
	// The first version of this test instead pointed the verifier at an unserved path. That
	// 404s, so it never reached the issuer comparison at all: it passed while testing
	// nothing. Found by an adversarial review pass, and it is the reason testjwks grew
	// SetDiscoveryIssuer rather than the test growing a cleverer URL.
	iss.SetDiscoveryIssuer("https://evil.example.com/realms/attacker")

	v := newVerifier(t, iss)

	err := errString(v.Verify(context.Background(), iss.Sign(iss.DefaultClaims())))
	if err == "" {
		t.Fatal("accepted a discovery document whose `issuer` did not match the configured issuer.\n\n" +
			"Anything able to influence the discovery response -- a redirect, a hijacked DNS " +
			"record, a misconfigured proxy -- can then point this service at a JWKS the attacker " +
			"controls, and every token they mint verifies.")
	}
	if !strings.Contains(err, "issuer mismatch") {
		t.Errorf("rejected for the wrong reason: %s\n\n"+
			"The test must fail on the issuer comparison, not on a 404 or a transport error, or "+
			"it is not testing the check it names.", err)
	}
}

// errString returns the error text from a Verify call, or "" when it succeeded.
func errString[T any](_ T, err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestRevokedKeysStopBeingTrusted covers the half of the caching rule that "refetch on a
// cache miss" silently omits.
//
// Refetching only on a MISS sounds sufficient and is not. When an issuer deletes a
// compromised key it keeps signing with its other, still-valid keys -- the normal shape at
// Keycloak, Google and Auth0, which all publish several at once. Every legitimate token then
// names a key id the cache already holds, so a miss never occurs, so no refetch ever occurs,
// and the deleted key stays trusted until the pod restarts. Anyone holding a token signed by
// the revoked key keeps authenticating indefinitely, and no metric shows anything wrong.
func TestRevokedKeysStopBeingTrusted(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	v := newVerifier(t, iss, func(o *auth.OIDCOptions) {
		o.MinRefresh = -1
		o.MaxKeyAge = time.Millisecond
	})

	// A token signed by the key that is about to be revoked.
	stolen := iss.Sign(iss.DefaultClaims())

	if _, err := v.Verify(context.Background(), stolen); err != nil {
		t.Fatalf("warming: %v", err)
	}

	// The issuer revokes it: ec-key-1 disappears from the published key set.
	iss.Rotate("ec-key-2")

	// Let the cached set age past MaxKeyAge.
	time.Sleep(20 * time.Millisecond)

	if _, err := v.Verify(context.Background(), stolen); err == nil {
		t.Fatal("a token signed by a REVOKED key was still accepted.\n\n" +
			"The issuer removed this key from its JWKS. Because the key id was already cached, " +
			"nothing ever refetched, so revocation never took effect -- the token keeps working " +
			"until the process restarts.")
	}
}

// TestSlowRefreshDoesNotStallCachedVerifications is an availability assertion, and the attack
// needs no credential at all.
//
// keyFunc runs before signature verification, so ANY caller can name an unknown key id and
// trigger a refetch. If the cache holds its write lock across that fetch, every concurrent
// request -- including ones whose key is cached and would have succeeded instantly -- blocks
// for as long as the IdP takes to answer. sync.RWMutex is not context-aware, so those
// requests ignore their own deadlines, and since auth runs inside the admission slot they
// consume the whole admission budget while they wait.
func TestSlowRefreshDoesNotStallCachedVerifications(t *testing.T) {
	t.Parallel()

	iss := testjwks.New(t)
	v := newVerifier(t, iss, func(o *auth.OIDCOptions) {
		o.MinRefresh = -1
		o.MaxKeyAge = -1 // no expiry, so a cached hit needs no refresh of its own
	})

	good := iss.Sign(iss.DefaultClaims())
	if _, err := v.Verify(context.Background(), good); err != nil {
		t.Fatalf("warming: %v", err)
	}

	// From here the JWKS endpoint is slow, standing in for a struggling IdP.
	const stall = 2 * time.Second
	iss.SetJWKSDelay(stall)

	// One unauthenticated request naming a key that does not exist, in flight.
	attack := make(chan struct{})
	go func() {
		defer close(attack)
		_, _ = v.Verify(context.Background(), iss.SignWithKid("does-not-exist", iss.DefaultClaims()))
	}()

	// Give the attacker's refresh time to be underway and holding whatever it holds.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	if _, err := v.Verify(context.Background(), good); err != nil {
		t.Fatalf("a cached-key verification failed during a slow refresh: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > stall/2 {
		t.Errorf("a verification against an ALREADY CACHED key took %v while an unrelated JWKS "+
			"fetch was in flight.\n\n"+
			"That means readers are blocked behind the fetch. One unauthenticated request naming "+
			"an unknown key id stalls every concurrent verification for the IdP's full response "+
			"time, ignoring their deadlines, while holding admission slots.", elapsed)
	}

	<-attack
}
