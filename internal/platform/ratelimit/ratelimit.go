// Package ratelimit is the distributed, per-tenant request quota.
//
// IT IS NOT THE ADMISSION LIMITER, and the two exist for different reasons:
//
//	admission (interceptor/admission.go)  LOCAL concurrency bound, sized from the database
//	                                      pool. Protects the process from doing more work
//	                                      than it can execute. Runs BEFORE auth, so a flood
//	                                      is shed without paying for signature verification.
//	ratelimit (here)                      DISTRIBUTED request quota per tenant, shared by
//	                                      every replica through Redis. Enforces a business
//	                                      policy -- "this customer bought 600 requests a
//	                                      minute" -- not a resource bound. Runs AFTER auth,
//	                                      because the tenant comes from the verified token.
//
// Shipping only the local one is the common mistake: a per-replica limit configured at
// "100rps" silently becomes 100 x replicas, resets to zero on every deploy, and changes
// meaning every time the HPA scales. It is a number that looks like a control and is not one.
package ratelimit

import (
	"context"
	"time"
)

// Result is the outcome of one Allow call.
type Result struct {
	// Allowed reports whether the request may proceed.
	Allowed bool

	// RetryAfter is how long until this key would be allowed again. Zero when Allowed.
	//
	// It is a computed value, not a guess: GCRA knows exactly when the next request fits.
	// Returning a real number is what lets a client back off correctly instead of retrying
	// immediately -- and immediate retries are the worst possible response to a limit.
	RetryAfter time.Duration

	// Remaining is the burst capacity left. Reported to clients as a header so a
	// well-behaved one can slow down BEFORE it is rejected.
	Remaining int
}

// Limiter decides whether one more request against a key is permitted.
//
// One method, because a limiter that also exposes Reset or Peek invites call sites to make
// decisions the limiter should be making.
type Limiter interface {
	Allow(ctx context.Context, key string) (Result, error)
}

// Config describes the quota.
type Config struct {
	// Limit is the sustained number of requests per Period.
	Limit int

	// Period is the window Limit applies to.
	Period time.Duration

	// Burst is how many requests may arrive instantaneously before throttling begins.
	//
	// Without burst, a client sending two requests back to back is rejected even at a
	// generous sustained rate, because GCRA spaces requests exactly Period/Limit apart.
	// Real clients are bursty; a limiter that ignores that rejects legitimate traffic and
	// gets configured away.
	Burst int
}

// EmissionInterval is the spacing between requests at the sustained rate.
func (c Config) EmissionInterval() time.Duration {
	if c.Limit <= 0 {
		return 0
	}
	return c.Period / time.Duration(c.Limit)
}

// DelayTolerance is how far ahead of the sustained schedule a caller may run.
//
// emission * burst, so exactly Burst requests are admitted instantaneously from an idle
// bucket. gcra_test.go asserts that boundary in both directions.
func (c Config) DelayTolerance() time.Duration {
	return c.EmissionInterval() * time.Duration(c.Burst)
}

// AllowAll is the limiter used when no Redis is configured.
//
// A NAMED TYPE rather than a nil Limiter the interceptor checks for. A nil check is one
// forgotten branch away from a nil-pointer panic on the request path, and "the limiter is
// absent" is a real configuration -- a single-replica service, or a deployment that limits at
// the ingress instead -- rather than an error state.
type AllowAll struct{}

// Allow always permits.
func (AllowAll) Allow(context.Context, string) (Result, error) {
	return Result{Allowed: true, Remaining: -1}, nil
}
