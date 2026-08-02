package client

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Retry describes when one method may be retried.
//
// RETRY IS OPT-IN, PER METHOD, and default-deny -- the same shape as the authorisation policy,
// for the same reason: only the person who wrote the method knows whether replaying it is
// safe, and a default that guesses will guess wrong on the one method where it matters.
//
// A blanket retry policy over a service is how a template ships duplicate orders. gRPC retries
// a request the server never answered AND a request the server answered with a retryable code
// after doing the work -- it cannot tell those apart. For a read that is free; for CreateOrder
// it is a second order, charged to a real customer, with no error anywhere.
//
// This repository has no idempotency-key mechanism (it was cut, see the README), so there is
// nothing that would make a mutation safe to replay. Mutations therefore get no retry policy
// at all, and that is written here rather than discovered.
type Retry struct {
	// Method is "package.Service/Method", without the leading slash -- "order.v1.OrderService/GetOrder".
	//
	// A bare "package.Service" applies to EVERY method of that service. Prefer naming
	// methods: the wildcard form silently covers methods added later, which is precisely
	// when a mutation slips into a retry policy nobody re-read.
	Method string

	// MaxAttempts counts the FIRST attempt, so 3 means one call plus two retries.
	//
	// grpc-go silently clamps this to 5 (its defaultMaxCallAttempts); asking for 10 gets you
	// 5 with no error and no log.
	MaxAttempts int

	// InitialBackoff and MaxBackoff bound the exponential wait.
	//
	// The jitter grpc-go actually applies is +/-20% around the exponential value, not the
	// "random between zero and the backoff" its own doc comment describes -- so two clients
	// retrying a shared dependency stay substantially in step. That is what RetryThrottling
	// on Options is for.
	InitialBackoff    time.Duration
	MaxBackoff        time.Duration
	BackoffMultiplier float64
}

// serviceConfig is the JSON grpc-go parses. It is built from typed values rather than written
// as a string literal, because a string literal is where the failure below comes from.
//
// EVERY FIELD NAME HERE IS LOAD-BEARING, and only some of them fail loudly. Measured against
// grpc-go v1.83.0 by breaking each one in turn:
//
//	"retrypolicy" for "retryPolicy"   ACCEPTED and works. Matching is case-insensitive,
//	                                  because grpc-go decodes into untagged structs.
//	"maxAttempt" for "maxAttempts"    REJECTED loudly at Dial: the field is required, so
//	                                  dropping it leaves a policy that fails validation --
//	                                  "the provided default service config is invalid".
//	"retry_policy" for "retryPolicy"  SILENT. An unrecognised key is discarded, the config
//	                                  is otherwise valid, Dial succeeds, and the connection
//	                                  has NO retry policy at all. No error, no warning, no
//	                                  log line.
//
// That last row is the one to fear, and nothing in a test suite notices it: a retry test that
// only asserts "the call eventually succeeded" passes, because the call succeeds on its first
// attempt whenever nothing is failing. Asking the CONNECTION what it ended up with is the only
// check that cannot be fooled -- TestARetryPolicyActuallyReachesTheConnection does exactly
// that, and it is the reason this file is generated from typed values rather than written as a
// JSON string literal.
type serviceConfig struct {
	MethodConfig    []methodConfig   `json:"methodConfig,omitempty"`
	RetryThrottling *retryThrottling `json:"retryThrottling,omitempty"`
}

type methodConfig struct {
	Name        []methodName `json:"name"`
	RetryPolicy *retryPolicy `json:"retryPolicy,omitempty"`
}

type methodName struct {
	Service string `json:"service"`
	Method  string `json:"method,omitempty"`
}

type retryPolicy struct {
	MaxAttempts int `json:"maxAttempts"`

	// Durations are STRINGS ending in "s" -- "0.1s", not "100ms" and not the number 0.1.
	// grpc-go parses these with its own protobuf-duration reader, not time.ParseDuration, and
	// a wrong form rejects the ENTIRE config rather than the one field.
	InitialBackoff       string   `json:"initialBackoff"`
	MaxBackoff           string   `json:"maxBackoff"`
	BackoffMultiplier    float64  `json:"backoffMultiplier"`
	RetryableStatusCodes []string `json:"retryableStatusCodes"`
}

type retryThrottling struct {
	MaxTokens  float64 `json:"maxTokens"`
	TokenRatio float64 `json:"tokenRatio"`
}

// retryableCodes is the allowlist, and it is deliberately one entry long.
//
// UNAVAILABLE is the only code that reliably means "this attempt did not happen": the
// connection failed, or the server is shutting down and said so. Retrying it is free.
//
// RESOURCE_EXHAUSTED is deliberately EXCLUDED even though it looks retryable, because this
// service's own server returns it for three different situations -- the admission limiter
// under load, the rate limiter, and a request whose deadline had already expired before it
// arrived. That last one can never succeed on a retry, and service config cannot express
// "retry unless the reason is DEADLINE_ALREADY_EXPIRED". Retrying the other two is also how a
// struggling upstream gets a traffic multiplier at its worst moment.
//
// DEADLINE_EXCEEDED is excluded for a harder reason: every attempt of a retried call shares
// ONE deadline, including the backoff sleeps. A call that just exhausted its deadline has
// nothing left to retry with, so listing it buys nothing and costs a confusing log line.
var retryableCodes = []string{"UNAVAILABLE"}

// buildServiceConfig renders the retry policies as grpc-go service config JSON.
func buildServiceConfig(retries []Retry, throttle bool) (string, error) {
	if len(retries) == 0 {
		return "", nil
	}

	cfg := serviceConfig{}

	seen := make(map[string]bool, len(retries))
	for _, r := range retries {
		service, method, err := splitMethod(r.Method)
		if err != nil {
			return "", err
		}

		// Duplicate names make grpc-go reject the WHOLE config with errDuplicatedName, which
		// would disable retries everywhere rather than in the one place with the typo.
		key := service + "/" + method
		if seen[key] {
			return "", fmt.Errorf("client: %q appears twice in the retry policy; grpc-go rejects "+
				"the entire service config on a duplicate, so every other method loses its "+
				"policy too", r.Method)
		}
		seen[key] = true

		if r.MaxAttempts < 2 {
			return "", fmt.Errorf("client: retry policy for %q has MaxAttempts %d; it counts the "+
				"first attempt, so anything below 2 means no retry at all and grpc-go rejects it",
				r.Method, r.MaxAttempts)
		}
		if r.InitialBackoff <= 0 || r.MaxBackoff <= 0 || r.BackoffMultiplier <= 0 {
			return "", fmt.Errorf("client: retry policy for %q needs a positive InitialBackoff, "+
				"MaxBackoff and BackoffMultiplier; grpc-go rejects the whole config otherwise", r.Method)
		}

		cfg.MethodConfig = append(cfg.MethodConfig, methodConfig{
			Name: []methodName{{Service: service, Method: method}},
			RetryPolicy: &retryPolicy{
				MaxAttempts:          r.MaxAttempts,
				InitialBackoff:       seconds(r.InitialBackoff),
				MaxBackoff:           seconds(r.MaxBackoff),
				BackoffMultiplier:    r.BackoffMultiplier,
				RetryableStatusCodes: retryableCodes,
			},
		})
	}

	if throttle {
		// RETRY THROTTLING, on by default whenever any retry policy exists.
		//
		// Without it, an upstream that starts failing receives MaxAttempts times its normal
		// load from every client at once -- the retry storm that turns a partial outage into
		// a total one. The token bucket is per-connection: each failure costs one token, each
		// success returns TokenRatio, and retries stop entirely below half the maximum.
		cfg.RetryThrottling = &retryThrottling{MaxTokens: 10, TokenRatio: 0.1}
	}

	b, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("client: render service config: %w", err)
	}
	return string(b), nil
}

// splitMethod parses "package.Service/Method" or a bare "package.Service".
func splitMethod(full string) (service, method string, err error) {
	trimmed := strings.TrimPrefix(full, "/")
	if trimmed == "" {
		return "", "", fmt.Errorf("client: empty method name in the retry policy")
	}

	service, method, found := strings.Cut(trimmed, "/")
	if service == "" {
		// grpc-go rejects a config naming a method with no service, and does it by failing
		// the whole config rather than the entry.
		return "", "", fmt.Errorf("client: %q names a method with no service", full)
	}
	if found && method == "" {
		return "", "", fmt.Errorf("client: %q has a trailing slash and no method name", full)
	}
	return service, method, nil
}

// seconds renders a duration the way grpc-go's service-config parser demands.
//
// Not time.Duration.String(): that produces "100ms" and "1m30s", both of which grpc-go
// rejects -- and rejecting one duration rejects the entire configuration.
func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "s"
}
