package grpcapi_test

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
	"github.com/example/gomicro/internal/platform/config"
	"github.com/example/gomicro/internal/testutil"
)

// TestRateLimitIsACTUALLYWIREDINTOTheChain is the "is it plugged in" test, and this repo has
// already been burned by not having one.
//
// interceptor/ratelimit_test.go proves the interceptor reacts correctly to a limiter, and
// ratelimit/gcra_test.go proves the limiter counts correctly. Neither proves the two are
// connected to the running server. That gap is exactly what hid the M5 auth bypass: a correct
// verifier existed and the server never called it.
//
// So this drives the REAL server, built by app.New from configuration, against a real
// (in-process) Redis, and requires a client to get throttled.
func TestRateLimitIsACTUALLYWIREDINTOTheChain(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)

	// A burst of 3, so the fourth call in the same tenant must be rejected. AUTH_MODE stays
	// dev, so every request arrives as the same dev-tenant principal and shares one bucket.
	conn := testutil.NewTestServer(t, func(c *config.Config) {
		c.Redis.Addr = mr.Addr()
		c.Redis.RateLimitPerMinute = 60
		c.Redis.RateLimitBurst = 3
	})
	client := orderv1.NewOrderServiceClient(conn)
	ctx := context.Background()

	var lastErr error
	allowed := 0
	for range 10 {
		_, err := client.ListOrders(ctx, &orderv1.ListOrdersRequest{})
		if err == nil {
			allowed++
			continue
		}
		lastErr = err
		break
	}

	if lastErr == nil {
		t.Fatalf("all 10 requests succeeded with a burst of 3.\n\n"+
			"The limiter is not in the chain. A correct limiter that nothing calls is exactly "+
			"the shape of the auth bypass this repo shipped in M5. (allowed=%d)", allowed)
	}
	if got := status.Code(lastErr); got != codes.ResourceExhausted {
		t.Fatalf("throttled request returned %v, want ResourceExhausted: %v", got, lastErr)
	}
	if allowed != 3 {
		t.Errorf("%d requests were allowed, want exactly the configured burst of 3", allowed)
	}
}

// TestNoRedisMeansNoThrottling confirms the off switch, so a service without Redis is not
// accidentally limited by a half-initialised limiter.
func TestNoRedisMeansNoThrottling(t *testing.T) {
	t.Parallel()

	// REDIS_ADDR unset is the default in testutil.TestConfig.
	client := orderv1.NewOrderServiceClient(testutil.NewTestServer(t))
	ctx := context.Background()

	for i := range 20 {
		if _, err := client.ListOrders(ctx, &orderv1.ListOrdersRequest{}); err != nil {
			t.Fatalf("request %d was rejected with no Redis configured: %v", i+1, err)
		}
	}
}

// TestAnUnreachableRedisDoesNotBreakTheService is the fail-open decision, end to end.
//
// Configured to use Redis, but Redis is dead. The service must keep serving: a quota store
// that takes the whole service down when it is unavailable has made itself a hard dependency
// of serving at all, which is strictly worse than not enforcing quotas for a few minutes.
func TestAnUnreachableRedisDoesNotBreakTheService(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close() // dead before the first request

	conn := testutil.NewTestServer(t, func(c *config.Config) {
		c.Redis.Addr = addr
		c.Redis.RateLimitPerMinute = 60
		c.Redis.RateLimitBurst = 1
	})
	client := orderv1.NewOrderServiceClient(conn)

	for i := range 5 {
		if _, err := client.ListOrders(context.Background(), &orderv1.ListOrdersRequest{}); err != nil {
			t.Fatalf("request %d failed because REDIS was down: %v\n\n"+
				"The limiter must fail open. Auth fails closed because it protects data; this "+
				"protects capacity, and refusing everything converts a degraded limiter into a "+
				"total outage.", i+1, err)
		}
	}
}
