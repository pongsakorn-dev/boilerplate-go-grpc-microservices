package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/example/gomicro/internal/platform/ratelimit"
)

// miniredis, not Docker.
//
// It speaks the Redis protocol in-process, runs the Lua script through a real interpreter,
// and -- the part that matters most here -- lets a test ADVANCE ITS CLOCK. Rate limiting is
// entirely about the passage of time, so a limiter tested only at t=0 has had half its
// behaviour checked: recovery, decay and retry-after are all invisible without a clock you
// can move.
//
// The alternative, sleeping for real seconds, makes the suite slow enough that people stop
// running it.

// clock drives BOTH of miniredis's clocks, which are independent and easy to confuse.
//
// Measured, because it cost a real debugging session:
//
//	mr.FastForward(d)  advances KEY EXPIRY only. redis.call('TIME') does not move at all.
//	mr.SetTime(t)      sets what TIME returns. TTLs do not move.
//
// A time-based limiter needs both. Driving only FastForward produces a test that looks like
// it waits and does not: the server clock the GCRA script reads stays put, so every
// "after waiting" assertion is really still measuring t=0.
//
// Worse, it can pass for the WRONG REASON. TestRejectionDoesNotExtendThePenalty first used
// FastForward alone and went green -- not because capacity had recovered, but because
// FastForward expired the key and handed the next request a brand-new bucket. The limiter
// could have been arbitrarily broken and that test would still have passed.
type clock struct {
	mr  *miniredis.Miniredis
	now time.Time
}

func (c *clock) advance(d time.Duration) {
	c.now = c.now.Add(d)
	c.mr.SetTime(c.now) // the limiter's clock, read by redis.call('TIME')
	c.mr.FastForward(d) // the keys' clock, which drives TTL expiry
}

func newLimiter(t *testing.T, cfg ratelimit.Config) (*ratelimit.RedisLimiter, *clock) {
	t.Helper()

	mr := miniredis.RunT(t)

	// Pin the clock up front so the test is deterministic and real elapsed time between
	// calls cannot leak into the measurement.
	start := time.Now()
	mr.SetTime(start)

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	limiter, err := ratelimit.NewRedis(client, cfg)
	if err != nil {
		t.Fatalf("NewRedis: %v", err)
	}
	return limiter, &clock{mr: mr, now: start}
}

// TestBurstIsExactlyTheConfiguredSize pins the boundary in BOTH directions.
//
// One direction alone is not enough: a limiter that admits everything passes "the first N are
// allowed", and one that admits nothing passes "request N+1 is rejected". Only asserting the
// exact transition distinguishes a working limiter from either broken one.
func TestBurstIsExactlyTheConfiguredSize(t *testing.T) {
	t.Parallel()

	const burst = 5
	limiter, _ := newLimiter(t, ratelimit.Config{Limit: 60, Period: time.Minute, Burst: burst})
	ctx := context.Background()

	for i := range burst {
		res, err := limiter.Allow(ctx, "tenant-a")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if !res.Allowed {
			t.Fatalf("request %d of %d was rejected; the burst allowance is smaller than "+
				"configured, so legitimate bursty clients get throttled", i+1, burst)
		}
	}

	res, err := limiter.Allow(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("request %d: %v", burst+1, err)
	}
	if res.Allowed {
		t.Fatalf("request %d was allowed with a burst of %d; the limiter admits more than "+
			"configured", burst+1, burst)
	}
	if res.RetryAfter <= 0 {
		t.Error("a rejection carries no RetryAfter, so a client has nothing to back off by " +
			"and will retry immediately -- turning a throttle into a flood")
	}
}

// TestCapacityRecoversAsTimePasses is the half a t=0 test cannot see.
func TestCapacityRecoversAsTimePasses(t *testing.T) {
	t.Parallel()

	// 60 per minute = one every second.
	limiter, clk := newLimiter(t, ratelimit.Config{Limit: 60, Period: time.Minute, Burst: 3})
	ctx := context.Background()

	for range 3 {
		if res, _ := limiter.Allow(ctx, "tenant-a"); !res.Allowed {
			t.Fatal("the initial burst was rejected")
		}
	}
	if res, _ := limiter.Allow(ctx, "tenant-a"); res.Allowed {
		t.Fatal("the burst was not exhausted")
	}

	// One emission interval later, exactly one more request fits.
	clk.advance(time.Second)

	if res, _ := limiter.Allow(ctx, "tenant-a"); !res.Allowed {
		t.Error("no capacity recovered after one emission interval, so the bucket never " +
			"refills and a tenant is throttled permanently after one burst")
	}
	if res, _ := limiter.Allow(ctx, "tenant-a"); res.Allowed {
		t.Error("two requests fit after one emission interval; capacity is recovering faster " +
			"than the configured rate")
	}
}

// TestRetryAfterIsAccurate checks the number is a computed answer, not a constant.
//
// A limiter that always says "retry in one second" is worse than useless under load: every
// rejected client returns at the same moment, so the retries arrive as a synchronised wave.
func TestRetryAfterIsAccurate(t *testing.T) {
	t.Parallel()

	limiter, clk := newLimiter(t, ratelimit.Config{Limit: 60, Period: time.Minute, Burst: 1})
	ctx := context.Background()

	if res, _ := limiter.Allow(ctx, "tenant-a"); !res.Allowed {
		t.Fatal("the first request was rejected")
	}

	rejected, err := limiter.Allow(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if rejected.Allowed {
		t.Fatal("the second request was allowed with burst 1")
	}

	// The emission interval is one second, so retry-after should be just under it.
	if rejected.RetryAfter > time.Second || rejected.RetryAfter < 900*time.Millisecond {
		t.Errorf("RetryAfter = %s, want just under the 1s emission interval.\n\n"+
			"A constant or wildly wrong value makes every rejected client come back at the "+
			"same moment.", rejected.RetryAfter)
	}

	// And waiting exactly that long is enough.
	clk.advance(rejected.RetryAfter)
	if res, _ := limiter.Allow(ctx, "tenant-a"); !res.Allowed {
		t.Errorf("still rejected after waiting the advertised RetryAfter of %s; the advice "+
			"the limiter gives clients is wrong", rejected.RetryAfter)
	}
}

// TestKeysAreIndependent is the multi-tenant property.
//
// One tenant exhausting its quota must not throttle another. Getting this wrong turns a
// single noisy customer into an outage for everybody, which is the exact failure a per-tenant
// limiter exists to prevent.
func TestKeysAreIndependent(t *testing.T) {
	t.Parallel()

	limiter, _ := newLimiter(t, ratelimit.Config{Limit: 60, Period: time.Minute, Burst: 2})
	ctx := context.Background()

	for range 3 {
		_, _ = limiter.Allow(ctx, "noisy-tenant")
	}
	if res, _ := limiter.Allow(ctx, "noisy-tenant"); res.Allowed {
		t.Fatal("the noisy tenant was not exhausted, so this proves nothing")
	}

	res, err := limiter.Allow(ctx, "quiet-tenant")
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !res.Allowed {
		t.Error("a quiet tenant was throttled because a different tenant exhausted ITS quota.\n\n" +
			"That is a shared bucket, not a per-tenant limit, and it turns one noisy customer " +
			"into an outage for everyone.")
	}
}

// TestRejectionDoesNotExtendThePenalty guards a subtle and punishing bug.
//
// If a rejected request still advanced the TAT, a client retrying in a tight loop would push
// its own recovery further away with every attempt and could be locked out indefinitely --
// while the server does work for every rejection. GCRA writes nothing on rejection precisely
// to avoid this.
func TestRejectionDoesNotExtendThePenalty(t *testing.T) {
	t.Parallel()

	limiter, clk := newLimiter(t, ratelimit.Config{Limit: 60, Period: time.Minute, Burst: 1})
	ctx := context.Background()

	if res, _ := limiter.Allow(ctx, "tenant-a"); !res.Allowed {
		t.Fatal("the first request was rejected")
	}

	// Hammer it while throttled.
	for range 50 {
		if res, _ := limiter.Allow(ctx, "tenant-a"); res.Allowed {
			t.Fatal("a request slipped through during the throttle window")
		}
	}

	// One emission interval after the ACCEPTED request, capacity should be back regardless
	// of how many rejections happened in between.
	clk.advance(time.Second)

	if res, _ := limiter.Allow(ctx, "tenant-a"); !res.Allowed {
		t.Error("50 rejected retries pushed the recovery time out.\n\n" +
			"A client retrying hard would extend its own lockout indefinitely, and the server " +
			"would keep paying for the rejections.")
	}
}

// TestRemainingReportsBurstCapacity supports the response headers a client uses to slow down
// BEFORE being rejected.
func TestRemainingReportsBurstCapacity(t *testing.T) {
	t.Parallel()

	const burst = 4
	limiter, _ := newLimiter(t, ratelimit.Config{Limit: 60, Period: time.Minute, Burst: burst})
	ctx := context.Background()

	for i := range burst {
		res, err := limiter.Allow(ctx, "tenant-a")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		want := burst - i - 1
		if res.Remaining != want {
			t.Errorf("after request %d, Remaining = %d, want %d", i+1, res.Remaining, want)
		}
	}
}

// TestIdleKeysExpire keeps the memory cost proportional to ACTIVE tenants.
//
// Without a TTL, every tenant that ever sent one request keeps a key forever, and a service
// with a long tail of customers slowly fills Redis with buckets that are permanently full of
// capacity nobody is using.
func TestIdleKeysExpire(t *testing.T) {
	t.Parallel()

	limiter, clk := newLimiter(t, ratelimit.Config{Limit: 60, Period: time.Minute, Burst: 2})
	ctx := context.Background()

	if _, err := limiter.Allow(ctx, "tenant-a"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if ttl := clk.mr.TTL("tenant-a"); ttl <= 0 {
		t.Fatalf("the key has no TTL (%s), so idle tenants accumulate forever", ttl)
	}

	// Long past the drain time.
	clk.advance(time.Minute)
	if clk.mr.Exists("tenant-a") {
		t.Error("the key survived well past its drain time")
	}
}

// TestConfigurationIsValidatedAtConstruction catches the settings that would silently reject
// everything or divide by zero.
func TestConfigurationIsValidatedAtConstruction(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	cases := []struct {
		name string
		cfg  ratelimit.Config
	}{
		{"zero limit", ratelimit.Config{Limit: 0, Period: time.Minute, Burst: 1}},
		{"negative limit", ratelimit.Config{Limit: -1, Period: time.Minute, Burst: 1}},
		{"zero period", ratelimit.Config{Limit: 10, Period: 0, Burst: 1}},
		// Burst zero admits NOTHING: the tolerance is zero, so even the first request from
		// an idle key arrives before its own TAT. A limiter rejecting 100% of traffic is
		// worth catching at construction, not in production.
		{"zero burst", ratelimit.Config{Limit: 10, Period: time.Minute, Burst: 0}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ratelimit.NewRedis(client, tc.cfg); err == nil {
				t.Errorf("%+v was accepted; it would reject every request or divide by zero", tc.cfg)
			}
		})
	}
}

// TestAllowAllIsUsableWithoutRedis covers the no-Redis configuration.
func TestAllowAllIsUsableWithoutRedis(t *testing.T) {
	t.Parallel()

	var limiter ratelimit.Limiter = ratelimit.AllowAll{}
	for range 100 {
		res, err := limiter.Allow(context.Background(), "anything")
		if err != nil || !res.Allowed {
			t.Fatalf("AllowAll rejected a request: allowed=%v err=%v", res.Allowed, err)
		}
	}
}
