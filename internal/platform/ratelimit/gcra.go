package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// GCRA -- the Generic Cell Rate Algorithm, borrowed from ATM traffic shaping.
//
// WHY NOT A TOKEN BUCKET. A token bucket needs two values per key (tokens remaining, last
// refill time) which must be read, computed and written atomically. GCRA needs ONE: the
// theoretical arrival time (TAT) of the next permitted request. One value means one GET and
// one SET, the whole decision fits in a short Lua script, and there is no read-modify-write
// race to reason about across replicas.
//
// It also answers "when may I retry?" exactly rather than approximately, because the TAT IS
// that answer. A limiter that cannot say when to come back leaves clients retrying
// immediately, which is the worst possible response to a limit and turns a throttle into a
// self-inflicted flood.
//
// The mechanism: each accepted request pushes TAT forward by one emission interval. A request
// is admitted if it arrives no earlier than TAT minus the burst tolerance. An idle key lets
// TAT fall back to now, which is what restores burst capacity over time.
const gcraScript = `
local key          = KEYS[1]
local emission_us  = tonumber(ARGV[1])
local tolerance_us = tonumber(ARGV[2])

-- REDIS's clock, not the caller's.
--
-- Every replica shares this one, so the limiter is immune to clock skew between pods. Using
-- the caller's time would make a pod whose clock runs two seconds fast enforce a different
-- window from its neighbours, and the resulting "the limit is wrong sometimes" is close to
-- undebuggable. Redis 7 replicates script EFFECTS rather than the script itself, so a
-- non-deterministic TIME call here is safe for replicas.
local t = redis.call('TIME')
local now_us = tonumber(t[1]) * 1000000 + tonumber(t[2])

local tat = redis.call('GET', key)
if tat == false then
  tat = now_us
else
  tat = tonumber(tat)
end

-- An idle key lets TAT decay back to now, which is how burst capacity is restored.
if tat < now_us then
  tat = now_us
end

local new_tat  = tat + emission_us
local allow_at = new_tat - tolerance_us
local diff     = now_us - allow_at

if diff < 0 then
  -- Rejected. Nothing is written, so a rejected request does not push the schedule further
  -- out -- otherwise a client retrying hard would extend its own penalty indefinitely.
  return {0, math.ceil(-diff), 0}
end

-- The TTL is exactly how long until this key drains, so idle tenants cost no memory.
local ttl_ms = math.ceil((new_tat - now_us) / 1000)
if ttl_ms < 1 then ttl_ms = 1 end
redis.call('SET', key, new_tat, 'PX', ttl_ms)

return {1, 0, math.floor(diff / emission_us)}
`

// RedisLimiter enforces a quota shared by every replica.
type RedisLimiter struct {
	client *redis.Client
	script *redis.Script
	cfg    Config

	emissionUS  int64
	toleranceUS int64
}

// NewRedis builds a limiter over an existing client.
func NewRedis(client *redis.Client, cfg Config) (*RedisLimiter, error) {
	if cfg.Limit <= 0 {
		return nil, fmt.Errorf("rate limit must be positive, got %d", cfg.Limit)
	}
	if cfg.Period <= 0 {
		return nil, fmt.Errorf("rate limit period must be positive, got %s", cfg.Period)
	}
	if cfg.Burst <= 0 {
		// Burst zero would admit nothing at all: the tolerance would be zero, so the very
		// first request from an idle key arrives before its own TAT and is rejected. A
		// limiter that rejects 100% of traffic is a configuration mistake worth catching at
		// construction rather than in production.
		return nil, fmt.Errorf("rate limit burst must be positive, got %d", cfg.Burst)
	}

	return &RedisLimiter{
		client:      client,
		script:      redis.NewScript(gcraScript),
		cfg:         cfg,
		emissionUS:  cfg.EmissionInterval().Microseconds(),
		toleranceUS: cfg.DelayTolerance().Microseconds(),
	}, nil
}

// Allow runs the GCRA decision for one key.
//
// An error from here means REDIS failed, not that the request is over quota. The caller
// decides what to do about that; see the fail-open note in interceptor/ratelimit.go.
func (l *RedisLimiter) Allow(ctx context.Context, key string) (Result, error) {
	res, err := l.script.Run(ctx, l.client, []string{key}, l.emissionUS, l.toleranceUS).Result()
	if err != nil {
		return Result{}, fmt.Errorf("rate limit script: %w", err)
	}

	values, ok := res.([]any)
	if !ok || len(values) != 3 {
		return Result{}, fmt.Errorf("rate limit script returned %T, want a 3-element array", res)
	}

	allowed, err := toInt64(values[0])
	if err != nil {
		return Result{}, fmt.Errorf("allowed: %w", err)
	}
	retryUS, err := toInt64(values[1])
	if err != nil {
		return Result{}, fmt.Errorf("retry after: %w", err)
	}
	remaining, err := toInt64(values[2])
	if err != nil {
		return Result{}, fmt.Errorf("remaining: %w", err)
	}

	return Result{
		Allowed:    allowed == 1,
		RetryAfter: time.Duration(retryUS) * time.Microsecond,
		Remaining:  int(remaining),
	}, nil
}

// Config reports the quota this limiter enforces, for reporting in headers.
func (l *RedisLimiter) Config() Config { return l.cfg }

// toInt64 normalises what a Lua script returns.
//
// Redis returns Lua numbers as integers, but go-redis surfaces them as int64 while some
// clients and miniredis versions produce other numeric types. Asserting one concrete type
// would work against a real Redis and fail against the test double, which is precisely the
// divergence the test double exists to avoid.
func toInt64(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case float64:
		return int64(n), nil
	default:
		return 0, fmt.Errorf("unexpected numeric type %T", v)
	}
}
