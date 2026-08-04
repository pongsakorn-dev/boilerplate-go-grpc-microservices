package client_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/example/gomicro/internal/platform/client"
)

// readRetry is the policy a read method gets.
func readRetry(method string) client.Retry {
	return client.Retry{
		Method:            method,
		MaxAttempts:       3,
		InitialBackoff:    10 * time.Millisecond,
		MaxBackoff:        50 * time.Millisecond,
		BackoffMultiplier: 2,
	}
}

// TestARetryPolicyActuallyReachesTheConnection is the guard against the quietest failure in
// this whole package.
//
// grpc-go decodes service config into untagged structs, so an unknown key is silently DROPPED.
// Misspell "retryPolicy", "maxAttempts" or "retryableStatusCodes" and you get a connection
// with no retries at all: no error, no warning, no log line -- and every retry test that only
// asserts "the call eventually succeeded" still passes, because the call succeeds on its first
// attempt in a test where nothing is failing.
//
// Asking the connection what it ended up with is the only check that cannot be fooled.
func TestARetryPolicyActuallyReachesTheConnection(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	conn := up.dial(t, func(o *client.Options) {
		o.Retry = []client.Retry{readRetry("order.v1.OrderService/GetOrder")}
	})

	// A real call first: NewClient is lazy, so the service config is not applied until the
	// connection is actually used.
	if err := invoke(context.Background(), conn, "order.v1.OrderService/GetOrder"); err != nil {
		t.Fatalf("call: %v", err)
	}

	mc := conn.GetMethodConfig("/order.v1.OrderService/GetOrder")
	if mc.RetryPolicy == nil {
		t.Fatal("the connection has NO retry policy for GetOrder.\n\n" +
			"The JSON was accepted and then ignored, which is what grpc-go does with a key it " +
			"does not recognise. Nothing else in a test suite would notice.")
	}
	if got := mc.RetryPolicy.MaxAttempts; got != 3 {
		t.Errorf("MaxAttempts = %d, want 3", got)
	}
	if !mc.RetryPolicy.RetryableStatusCodes[codes.Unavailable] {
		t.Errorf("UNAVAILABLE is not in the retryable set: %v", mc.RetryPolicy.RetryableStatusCodes)
	}

	// The codes deliberately left OUT matter as much as the one left in.
	for _, code := range []codes.Code{codes.ResourceExhausted, codes.DeadlineExceeded, codes.Internal} {
		if mc.RetryPolicy.RetryableStatusCodes[code] {
			t.Errorf("%s is retryable; see retryableCodes for why each of these is excluded", code)
		}
	}
}

// TestAReadIsRetried proves the policy has teeth, by counting arrivals at the callee.
func TestAReadIsRetried(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, unavailableTimes(2))
	conn := up.dial(t, func(o *client.Options) {
		o.Retry = []client.Retry{readRetry("order.v1.OrderService/GetOrder")}
	})

	if err := invoke(context.Background(), conn, "order.v1.OrderService/GetOrder"); err != nil {
		t.Fatalf("the call failed despite a 3-attempt policy and 2 failures: %v", err)
	}
	if n := up.count(); n != 3 {
		t.Errorf("the upstream saw %d attempts, want 3", n)
	}
}

// TestAMethodWithNoPolicyIsNotRetried is the default, and the one that protects mutations.
//
// gRPC cannot distinguish a request the server never received from one it received, acted on,
// and then failed to acknowledge. Retrying the second is a duplicate order. Since this
// repository ships no idempotency-key mechanism, there is nothing that would make replaying a
// mutation safe -- so mutations are simply absent from the policy, and this test proves that
// absence means what it should.
func TestAMethodWithNoPolicyIsNotRetried(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, unavailableTimes(2))
	conn := up.dial(t, func(o *client.Options) {
		// GetOrder is retryable. CreateOrder is deliberately not listed.
		o.Retry = []client.Retry{readRetry("order.v1.OrderService/GetOrder")}
	})

	err := invoke(context.Background(), conn, "order.v1.OrderService/CreateOrder")
	if err == nil {
		t.Fatal("CreateOrder succeeded despite the upstream failing")
	}
	// Read the upstream's code off the *client.Error, NOT with status.Code(err).
	//
	// This line used to be `codeOf(err) != codes.Unavailable`, and it passed -- which was
	// itself the evidence for a bug. status.Code walks the chain, so it could only have
	// answered Unavailable by reaching through the *client.Error to the upstream's status.
	// That is the exact leak the type exists to prevent, demonstrated in this package's own
	// test suite. Asking the type directly says what the test means and cannot regress.
	upErr, ok := client.From(err)
	if !ok {
		t.Fatalf("err is not a *client.Error: %T (%v)", err, err)
	}
	if upErr.Code != codes.Unavailable {
		t.Errorf("upstream code = %s, want Unavailable", upErr.Code)
	}
	if n := up.count(); n != 1 {
		t.Errorf("CreateOrder reached the upstream %d times, want exactly 1.\n\n"+
			"A retried mutation is a duplicate order: gRPC cannot tell a request the server "+
			"never saw from one it acted on and failed to acknowledge.", n)
	}

	if mc := conn.GetMethodConfig("/order.v1.OrderService/CreateOrder"); mc.RetryPolicy != nil {
		t.Errorf("CreateOrder has a retry policy (%+v); only methods named in Options.Retry "+
			"may have one", mc.RetryPolicy)
	}
}

// TestANonRetryableCodeIsNotRetried covers the other axis: the method is opted in, but the
// failure is not one a retry can fix.
func TestANonRetryableCodeIsNotRetried(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, func(int) error {
		// The shape the server's rate limiter and admission limiter both return -- and one
		// of the three things it uses ResourceExhausted for can never succeed on a retry.
		return status.Error(codes.ResourceExhausted, "rate limited")
	})
	conn := up.dial(t, func(o *client.Options) {
		o.Retry = []client.Retry{readRetry("order.v1.OrderService/GetOrder")}
	})

	if err := invoke(context.Background(), conn, "order.v1.OrderService/GetOrder"); err == nil {
		t.Fatal("the call succeeded")
	}
	if n := up.count(); n != 1 {
		t.Errorf("a ResourceExhausted response was retried %d times.\n\n"+
			"Retrying an overloaded upstream multiplies the load that overloaded it.", n)
	}
}

// TestAMalformedRetryPolicyFailsLoudly turns a whole class of silent misconfiguration into a
// startup error.
//
// grpc-go rejects the ENTIRE service config on one bad entry, so a single typo would otherwise
// remove retries from every method on the connection.
func TestAMalformedRetryPolicyFailsLoudly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		retry client.Retry
	}{
		{"max attempts below two", client.Retry{
			Method: "order.v1.OrderService/GetOrder", MaxAttempts: 1,
			InitialBackoff: time.Second, MaxBackoff: time.Second, BackoffMultiplier: 2,
		}},
		{"no backoff", client.Retry{
			Method: "order.v1.OrderService/GetOrder", MaxAttempts: 3, BackoffMultiplier: 2,
		}},
		{"no multiplier", client.Retry{
			Method: "order.v1.OrderService/GetOrder", MaxAttempts: 3,
			InitialBackoff: time.Second, MaxBackoff: time.Second,
		}},
		{"empty method", client.Retry{
			MaxAttempts: 3, InitialBackoff: time.Second, MaxBackoff: time.Second, BackoffMultiplier: 2,
		}},
		{"trailing slash", client.Retry{
			Method: "order.v1.OrderService/", MaxAttempts: 3,
			InitialBackoff: time.Second, MaxBackoff: time.Second, BackoffMultiplier: 2,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := testConfig(t)
			opts := client.New(cfg, "passthrough:///upstream")
			opts.TransportCredentials = client.Insecure()
			opts.Retry = []client.Retry{tc.retry}

			conn, err := client.Dial(cfg, opts)
			if err == nil {
				_ = conn.Close()
				t.Error("an invalid retry policy was accepted; grpc-go would reject the whole " +
					"service config and silently leave every method without retries")
			}
		})
	}
}

// TestADuplicateMethodIsRejected covers the specific mistake that disables retries everywhere.
func TestADuplicateMethodIsRejected(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	opts := client.New(cfg, "passthrough:///upstream")
	opts.TransportCredentials = client.Insecure()
	opts.Retry = []client.Retry{
		readRetry("order.v1.OrderService/GetOrder"),
		readRetry("/order.v1.OrderService/GetOrder"), // same method, written with a slash
	}

	conn, err := client.Dial(cfg, opts)
	if err == nil {
		_ = conn.Close()
		t.Fatal("a duplicated method was accepted. grpc-go answers a duplicate by rejecting " +
			"the entire service config, so every OTHER method would lose its policy too")
	}
}

// TestRetriesShareTheCallersDeadline records a property people expect to be false.
//
// Every attempt of a retried call, and every backoff sleep between them, comes out of ONE
// deadline. There is no per-attempt refresh. A three-attempt policy inside a tight budget does
// not get three chances; it gets one deadline divided three ways.
func TestRetriesShareTheCallersDeadline(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, unavailableTimes(2))
	conn := up.dial(t, func(o *client.Options) {
		o.Retry = []client.Retry{{
			Method:            "order.v1.OrderService/GetOrder",
			MaxAttempts:       3,
			InitialBackoff:    400 * time.Millisecond,
			MaxBackoff:        400 * time.Millisecond,
			BackoffMultiplier: 1,
		}}
	})

	// Enough for one attempt and part of a backoff, nowhere near three attempts.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := invoke(ctx, conn, "order.v1.OrderService/GetOrder")
	if err == nil {
		t.Fatal("the call succeeded; the deadline should have run out mid-backoff")
	}
	if n := up.count(); n >= 3 {
		t.Errorf("the upstream saw %d attempts inside a deadline too short for them.\n\n"+
			"Retries do NOT get a fresh deadline each; they share the caller's.", n)
	}

}
