package grpcapi_test

import (
	"context"
	"testing"
	"time"

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

// TestOpeningAStreamSpendsQuotaToo closes the cheapest way around the quota above.
//
// The stream chain had no rate limiting at all, and WatchOrders runs a List against the
// database on every open. So the throttled tenant in the test above could keep issuing the
// same query, without limit, by asking for it as a stream instead. A quota that a caller can
// step around by choosing a different RPC for the same data is not a quota.
//
// The subtlety worth naming: a stream OPEN is the billable event here, not a stream message.
// WatchOrders sends a snapshot and then idles, so per-message billing would measure how long
// a client stayed connected rather than how much work it asked for.
func TestOpeningAStreamSpendsQuotaToo(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)

	conn := testutil.NewTestServer(t, func(c *config.Config) {
		c.Redis.Addr = mr.Addr()
		c.Redis.RateLimitPerMinute = 60
		c.Redis.RateLimitBurst = 2
	})
	client := orderv1.NewOrderServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Opening a stream returns before the server has necessarily run the chain, so the
	// rejection surfaces on the first Recv rather than from WatchOrders itself.
	var lastErr error
	opened := 0
	for range 10 {
		stream, err := client.WatchOrders(ctx, &orderv1.WatchOrdersRequest{})
		if err == nil {
			_, err = stream.Recv()
		}
		if err == nil {
			opened++
			continue
		}
		lastErr = err
		break
	}

	if lastErr == nil {
		t.Fatalf("all 10 stream opens succeeded with a burst of 2.\n\n"+
			"The stream chain is not enforcing the quota, so a tenant that has exhausted its "+
			"limit on ListOrders can keep running the same query by opening WatchOrders "+
			"instead. (opened=%d)", opened)
	}
	if got := status.Code(lastErr); got != codes.ResourceExhausted {
		t.Fatalf("throttled stream returned %v, want ResourceExhausted: %v", got, lastErr)
	}
	if opened != 2 {
		t.Errorf("%d streams opened, want exactly the configured burst of 2", opened)
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
