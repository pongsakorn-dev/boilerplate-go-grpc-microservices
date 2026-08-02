package interceptor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"

	"github.com/example/gomicro/internal/platform/apperr"
	"github.com/example/gomicro/internal/platform/auth"
	"github.com/example/gomicro/internal/platform/ratelimit"
)

// stubLimiter is a Limiter whose answer the test dictates, so the interceptor's REACTION can
// be tested independently of GCRA's arithmetic (which gcra_test.go covers against miniredis).
type stubLimiter struct {
	result ratelimit.Result
	err    error

	keys []string
}

func (s *stubLimiter) Allow(_ context.Context, key string) (ratelimit.Result, error) {
	s.keys = append(s.keys, key)
	return s.result, s.err
}

func withPrincipal(tenant string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		Subject:  "user-1",
		TenantID: tenant,
	})
}

func okHandler(called *bool) grpc.UnaryHandler {
	return func(context.Context, any) (any, error) {
		*called = true
		return "ok", nil
	}
}

// TestLimiterFailsOPENWhenRedisIsDown is the decision this interceptor turns on, and it is
// the OPPOSITE of what auth does two positions up the chain.
//
// The difference is what each protects. Auth protects DATA: without it a request may read
// something it must not, so a failure must deny. The limiter protects CAPACITY: without it,
// requests are merely unthrottled. Failing closed here would let an unreachable Redis reject
// every request on every replica -- turning "quotas are briefly unenforced" into a total
// outage, and making the quota store a hard dependency of serving at all.
func TestLimiterFailsOPENWhenRedisIsDown(t *testing.T) {
	t.Parallel()

	limiter := &stubLimiter{err: errors.New("dial tcp: connection refused")}
	log, buf := captureLogs()
	intercept := RateLimit(limiter, log)

	var called bool
	resp, err := intercept(withPrincipal("tenant-a"), nil, testInfo, okHandler(&called))

	if err != nil {
		t.Fatalf("the request was rejected because the LIMITER was unavailable: %v\n\n"+
			"An unreachable Redis must not take the service down with it.", err)
	}
	if !called {
		t.Error("the handler never ran")
	}
	if resp != "ok" {
		t.Errorf("resp = %v, want the handler's response", resp)
	}

	// Loud, though. A limiter that has been silently off for a month is something you find
	// out about from a bill.
	if !strings.Contains(buf.String(), "rate limiter unavailable") {
		t.Errorf("failing open was not logged, so nobody would ever learn quotas stopped "+
			"being enforced:\n%s", buf.String())
	}
}

// TestRejectionCarriesActionableRetryAdvice checks the client is told when to come back.
//
// Without RetryInfo a well-behaved gRPC client retries immediately, and the retries become
// the load -- a throttle that converts itself into a flood.
func TestRejectionCarriesActionableRetryAdvice(t *testing.T) {
	t.Parallel()

	limiter := &stubLimiter{result: ratelimit.Result{Allowed: false, RetryAfter: 2500 * time.Millisecond}}
	log, _ := captureLogs()
	intercept := RateLimit(limiter, log)

	var called bool
	_, err := intercept(withPrincipal("tenant-a"), nil, testInfo, okHandler(&called))

	if err == nil {
		t.Fatal("an over-quota request was allowed through")
	}
	if called {
		t.Error("the handler ran for a rejected request")
	}

	appErr, ok := apperr.From(err)
	if !ok {
		t.Fatalf("the error is not an *apperr.Error (%T), so ErrorMap cannot map it and the "+
			"client sees codes.Unknown", err)
	}
	if appErr.Kind != apperr.KindResourceExhausted {
		t.Errorf("kind = %v, want ResourceExhausted", appErr.Kind)
	}

	var retryInfo *errdetails.RetryInfo
	for _, d := range appErr.Details {
		if ri, isRetry := d.(*errdetails.RetryInfo); isRetry {
			retryInfo = ri
		}
	}
	if retryInfo == nil {
		t.Fatal("no RetryInfo detail. Clients and service meshes read it to schedule a retry; " +
			"without it they come back immediately and the retries become the load.")
	}
	if got := retryInfo.GetRetryDelay().AsDuration(); got != 2500*time.Millisecond {
		t.Errorf("RetryDelay = %s, want the limiter's computed 2.5s -- not a constant", got)
	}
}

// TestUnauthenticatedRequestsAreNotLimited protects the liveness probe.
//
// Health is a Public method, so it reaches this interceptor with no principal. Rate limiting
// it would throttle the kubelet -- which probes once a second from three probes on every pod
// -- and a service that fails its own readiness check removes itself from its Service
// endpoints. Self-inflicted outage, caused by a limiter working as configured.
//
// Unauthenticated floods are not unprotected: Admission runs BEFORE auth and sheds them
// without paying for signature verification. Two mechanisms, two positions, two jobs.
func TestUnauthenticatedRequestsAreNotLimited(t *testing.T) {
	t.Parallel()

	// A limiter that would reject everything, if it were consulted.
	limiter := &stubLimiter{result: ratelimit.Result{Allowed: false, RetryAfter: time.Minute}}
	log, _ := captureLogs()
	intercept := RateLimit(limiter, log)

	var called bool
	_, err := intercept(context.Background(), nil, testInfo, okHandler(&called))

	if err != nil {
		t.Fatalf("a request with no principal was rate limited: %v\n\n"+
			"grpc.health.v1.Health is Public and arrives here without one. Throttling it makes "+
			"every pod fail readiness and leave its own Service.", err)
	}
	if !called {
		t.Error("the handler never ran")
	}
	if len(limiter.keys) != 0 {
		t.Errorf("the limiter was consulted for an unauthenticated request: %v", limiter.keys)
	}
}

// TestTheKeyScopesToTenantAndMethod pins the quota's granularity.
//
// The tenant must be in the key or this is a global limit that one noisy customer exhausts
// for everybody. The method must be there too -- see the trade-off recorded on limitKey.
func TestTheKeyScopesToTenantAndMethod(t *testing.T) {
	t.Parallel()

	limiter := &stubLimiter{result: ratelimit.Result{Allowed: true, Remaining: 5}}
	log, _ := captureLogs()
	intercept := RateLimit(limiter, log)

	var called bool
	if _, err := intercept(withPrincipal("acme"), nil, testInfo, okHandler(&called)); err != nil {
		t.Fatalf("Allow: %v", err)
	}

	if len(limiter.keys) != 1 {
		t.Fatalf("the limiter was consulted %d times for one request", len(limiter.keys))
	}
	key := limiter.keys[0]

	if !strings.Contains(key, "acme") {
		t.Errorf("key %q does not contain the tenant, so this is a GLOBAL limit and one "+
			"customer can exhaust it for everyone", key)
	}
	if !strings.Contains(key, testInfo.FullMethod) {
		t.Errorf("key %q does not contain the method", key)
	}
}

// TestDifferentTenantsGetDifferentKeys is the property the previous test implies but does not
// prove: containing the tenant is not the same as varying by it.
func TestDifferentTenantsGetDifferentKeys(t *testing.T) {
	t.Parallel()

	limiter := &stubLimiter{result: ratelimit.Result{Allowed: true}}
	log, _ := captureLogs()
	intercept := RateLimit(limiter, log)

	var called bool
	_, _ = intercept(withPrincipal("tenant-a"), nil, testInfo, okHandler(&called))
	_, _ = intercept(withPrincipal("tenant-b"), nil, testInfo, okHandler(&called))

	if len(limiter.keys) != 2 {
		t.Fatalf("got %d keys, want 2", len(limiter.keys))
	}
	if limiter.keys[0] == limiter.keys[1] {
		t.Errorf("both tenants produced the key %q; they share one bucket", limiter.keys[0])
	}
}

// TestAllowAllIsTransparent confirms the no-Redis path costs the request nothing.
func TestAllowAllIsTransparent(t *testing.T) {
	t.Parallel()

	log, _ := captureLogs()
	intercept := RateLimit(ratelimit.AllowAll{}, log)

	var called bool
	resp, err := intercept(withPrincipal("tenant-a"), nil, testInfo, okHandler(&called))
	if err != nil || !called || resp != "ok" {
		t.Errorf("AllowAll interfered with the request: resp=%v called=%v err=%v", resp, called, err)
	}
}
