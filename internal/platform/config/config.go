// Package config is the ONLY package that reads the environment.
//
// Everything else receives a Config value. That single rule is what makes the service
// testable without t.Setenv, keeps configuration auditable in one file, and means a
// misconfiguration is caught by Validate() before a single listener opens rather than
// surfacing as a nil pointer three minutes into a rollout.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment names.
const (
	EnvDevelopment = "development"
	EnvStaging     = "staging"
	EnvProduction  = "production"
)

// Store drivers.
const (
	StoreMemory   = "memory"
	StorePostgres = "postgres"
)

// Auth modes.
const (
	AuthDev  = "dev"
	AuthOIDC = "oidc"
)

// Role is which binary is reading this configuration.
//
// It exists because one Config serves two processes with genuinely different requirements,
// and validating both against the server's rules produced a real failure: cmd/worker could
// not start with APP_ENV=production, because AUTH_MODE defaults to dev and dev is refused in
// production. That refusal is right for a process that serves traffic and meaningless for one
// that opens no listener -- the worker verifies no tokens, so there is nothing to bypass.
//
// It was found by writing deploy/k8s/base/worker.yaml, not by reading the code.
//
// THE ROLE COMES FROM THE BINARY, never from the environment. An APP_ROLE variable would move
// the trap rather than remove it: a worker deployed without it would fail exactly as before,
// and the manifest that forgot it is the manifest nobody reviews. cmd/worker calls LoadWorker
// and cannot get this wrong.
type Role int

const (
	// RoleServer serves traffic. Auth settings are load-bearing.
	RoleServer Role = iota

	// RoleWorker drains the outbox and consumes the broker. It has no listener.
	RoleWorker
)

// Secret wraps a value that must never reach a log, an error string, or a crash dump.
//
// It implements Stringer, json.Marshaler and slog.LogValuer, which are the three routes
// by which a password normally escapes: fmt.Printf("%+v", cfg), a JSON dump of config,
// and structured logging. Covering only one of the three is the usual mistake.
type Secret string

func (s Secret) String() string               { return "[REDACTED]" }
func (s Secret) GoString() string             { return "[REDACTED]" }
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }
func (s Secret) LogValue() any                { return "[REDACTED]" }
func (s Secret) Reveal() string               { return string(s) }

// Config is the full runtime configuration.
type Config struct {
	// Role is set by the binary, not the environment. See the Role type.
	Role Role

	AppEnv      string
	ServiceName string
	Version     string

	// Three listeners.
	//
	// GRPCAddr and GatewayAddr are public; AdminAddr is NOT. Admin carries /metrics and
	// /debug/pprof, and net/http/pprof registers itself on http.DefaultServeMux in an
	// init() function. Serving that on an ingress port hands anyone a heap dumper and a
	// CPU-profiler trigger. observability/admin_test.go asserts the split.
	GRPCAddr  string
	AdminAddr string

	// GatewayAddr serves HTTP+JSON transcoded onto the same gRPC service.
	//
	// Empty DISABLES the REST edge entirely, and that is a supported configuration rather
	// than an oversight: a service with only gRPC clients should not expose an HTTP surface
	// it never uses. gateway_test.go asserts the disabled case really binds nothing.
	GatewayAddr string

	LogLevel  string
	LogFormat string

	StoreDriver string
	Postgres    PostgresConfig

	AuthMode string
	OIDC     OIDCConfig

	Redis  RedisConfig
	Outbox OutboxConfig
	NATS   NATSConfig

	// Retention bounds how long the two append-only tables keep rows. See cmd/prune.
	Retention RetentionConfig

	// Upstream configures OUTBOUND calls to other services. See internal/platform/client.
	Upstream UpstreamConfig

	Telemetry TelemetryConfig
	Server    ServerConfig
	Shutdown  ShutdownConfig
}

type TelemetryConfig struct {
	// OTLPEndpoint is the collector address, e.g. "localhost:4317". Empty means tracing is
	// instrumented everywhere but exports nowhere -- which is what lets a fresh clone run
	// with no collector at all.
	OTLPEndpoint string

	// TraceSampleRatio is the head-sampling ratio for traces with no sampled parent.
	//
	// 1.0 in development so you see everything. In production this is the main cost lever:
	// at high request rates, tracing every request costs more than running the service.
	TraceSampleRatio float64
}

type PostgresConfig struct {
	DSN Secret

	// MaxOpenConns must be explicit.
	//
	// Pool defaults derive from the machine's CPU count, which inside a container
	// reflects the NODE's cores, not the cgroup limit. Twenty replicas each defaulting to
	// a large pool will exhaust a stock max_connections of 100 long before any of them is
	// busy, and the failure looks like a database outage rather than a config mistake.
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

type OIDCConfig struct {
	IssuerURL string
	Audience  string

	// JWKSURL overrides discovery. Empty is the normal case: the verifier reads
	// jwks_uri from the issuer's /.well-known/openid-configuration.
	JWKSURL string

	// Leeway tolerates clock skew between this service and the identity provider.
	Leeway time.Duration

	// TenantClaim and ScopeClaim are where this provider puts the tenant and the granted
	// scopes. Configurable because no two providers agree, and hardcoding one vendor's
	// shape is what makes a "generic" template single-vendor in practice.
	//
	// Both accept dotted paths for nested claims ("realm_access.roles"). The defaults suit
	// the Keycloak realm in deploy/keycloak/; auth/claims.go carries the mapping table for
	// Auth0, Cognito and Entra ID.
	TenantClaim string
	ScopeClaim  string

	// ServiceClaim marks a machine caller, for providers that do not follow RFC 9068's
	// sub==client_id convention -- which real Keycloak service accounts do not. See
	// auth/claims.go for the token that proved it.
	ServiceClaim string

	// MaxKeyAge bounds how long a cached JWKS is served before revalidation.
	//
	// This is what makes key REVOCATION take effect. Refetching only when a key id is
	// unknown never revalidates in the common case, because an issuer that revokes one key
	// keeps signing with its others -- so every token names a key already cached, and the
	// revoked one stays trusted until the process restarts.
	MaxKeyAge time.Duration
}

// RedisConfig configures the distributed rate limiter.
//
// Redis is used for QUOTAS ONLY. Cache-aside was deliberately cut from this template: a cache
// is easy to add and hard to get right, its correctness depends entirely on the invalidation
// rules of the data being cached, and shipping a generic one teaches a pattern that is wrong
// for most domains. Rate limiting has no such ambiguity -- the semantics are the same
// everywhere.
type RedisConfig struct {
	// Addr is host:port. EMPTY DISABLES rate limiting entirely, and that is a supported
	// configuration: a single-replica service, or one whose quotas are enforced at the
	// ingress, should not be forced to run Redis. app.New substitutes ratelimit.AllowAll,
	// which says so in the type rather than by being nil.
	Addr string

	Password Secret
	DB       int

	// RateLimitPerMinute is the sustained per-tenant, per-method request quota.
	RateLimitPerMinute int

	// RateLimitBurst is how many requests may arrive instantaneously.
	//
	// Without burst, two requests back to back are rejected even at a generous sustained
	// rate, because GCRA spaces them exactly Period/Limit apart. Real clients are bursty; a
	// limiter that ignores that rejects legitimate traffic and gets configured away.
	RateLimitBurst int
}

// OutboxConfig configures the relay in cmd/worker.
//
// Read by the WORKER, not the API server: the server writes outbox rows inside its business
// transactions and never drains them. Keeping the settings here anyway means one config
// package for the whole system, which is the rule that makes a misconfiguration a startup
// error instead of a nil pointer.
type OutboxConfig struct {
	// BatchSize bounds one claim.
	//
	// The claim transaction stays open while every message in the batch is published, so
	// this is directly how long a database transaction is held across network I/O. Large
	// batches amortise round trips and bloat vacuum; small ones do the opposite.
	BatchSize int

	// PollInterval is the wait between drains that found nothing.
	//
	// A drain that filled its batch goes again IMMEDIATELY rather than waiting, so this
	// bounds idle latency, not throughput: a backlog clears at full speed regardless.
	PollInterval time.Duration

	// MaxConns bounds the relay's database pool. Small on purpose -- background work should
	// not compete with request-serving replicas for the database's connection budget.
	MaxConns int

	// ObserveInterval is how often outbox.Observer refreshes its gauges.
	//
	// DELIBERATELY NOT PollInterval, even though both are timers in the same process. The
	// observer runs on its own clock precisely so that a relay wedged on an unresponsive
	// broker cannot stop the metrics that would report it: sharing the relay's tick would
	// freeze the oldest-pending-age gauge at the moment it started mattering.
	//
	// Default 15s, matching a conventional Prometheus scrape interval -- refreshing faster
	// than the scrape only costs database round trips nobody reads.
	ObserveInterval time.Duration
}

// RetentionConfig bounds the two tables that otherwise grow forever.
//
// Both are append-only by design: the outbox keeps every event it has ever published, and
// processed_events keeps one row per message ever consumed. Neither is ever read again after
// its purpose is served, and nothing in the system deletes them, so on a busy service they
// become the largest tables in the database with no upper bound.
type RetentionConfig struct {
	// Outbox is how long a PUBLISHED row is kept before cmd/prune deletes it.
	//
	// Published rows are pure history: the relay will never look at them again, since its
	// claim query filters on published_at IS NULL. They are kept at all only so an operator
	// investigating an incident can see what was sent and when.
	//
	// QUARANTINED rows are NEVER deleted by retention, whatever this is set to. A row with
	// failed_at set has not been published and never will be until a human clears it -- so
	// ageing it out would silently discard the one event anybody actually needed to see.
	Outbox time.Duration

	// ProcessedEvents is how long a consumer deduplication row is kept.
	//
	// THIS VALUE IS A CORRECTNESS BOUNDARY, not a housekeeping preference, and Validate
	// refuses a configuration where it does not exceed NATS_STREAM_MAX_AGE.
	//
	// The dedup row is what stops a redelivered message being applied twice. It is safe to
	// delete only once the broker can no longer deliver that message at all -- which is
	// exactly when the stream's own retention has dropped it. Delete it one hour early and a
	// redelivery lands on a consumer with no memory of having seen it: the projection applies
	// the event a second time, the count is wrong, and it stays wrong.
	ProcessedEvents time.Duration

	// BatchSize bounds one DELETE statement.
	//
	// A single unbounded DELETE over months of accumulated rows takes a lock for its whole
	// duration and generates one enormous WAL record. Batching keeps each statement short
	// enough that ordinary traffic is not blocked behind it.
	BatchSize int
}

// NATSConfig configures the JetStream publisher and consumer.
//
// The tenant is deliberately ABSENT from every subject setting here, and that is the one
// decision in this struct worth reading twice. See internal/platform/events for the measured
// reason: a tenant id containing a dot -- "acme.com", an entirely ordinary value -- becomes
// extra subject tokens and lands inside another tenant's subject subtree.
type NATSConfig struct {
	// URL is the NATS server, e.g. "nats://localhost:4222".
	//
	// EMPTY DISABLES the broker, and that is a supported configuration rather than a
	// degraded one: cmd/worker falls back to outbox.LogPublisher, which prints exactly what
	// would be published. A fresh clone therefore runs the whole outbox path -- claiming,
	// batching, the marking transaction -- with no broker installed.
	URL string

	// Stream is the JetStream stream name. Created or updated at startup, idempotently.
	Stream string

	// SubjectPrefix is prepended to the event type: "events" + "order.created" becomes the
	// subject "events.order.created".
	SubjectPrefix string

	// DLQSubjectPrefix receives messages this consumer gives up on.
	//
	// JetStream has NO built-in dead-letter queue. Exceeding MaxDeliver merely stops
	// redelivery and emits an advisory that nothing consumes by default -- so without this,
	// a message that fails MaxDeliver times is silently gone. The consumer republishes under
	// this prefix and only then terminates the original.
	DLQSubjectPrefix string

	// DuplicateWindow is how long JetStream remembers a Nats-Msg-Id.
	//
	// This is what collapses the relay's at-least-once republishing into one stored message.
	// A republish that arrives AFTER the window is a genuine duplicate the consumer must
	// handle, which is why processed_events exists as well -- the window is an optimisation,
	// not the correctness boundary.
	DuplicateWindow time.Duration

	// StreamMaxAge bounds how long the stream keeps messages. An unbounded stream is a disk
	// that fills at 03:00 on a date nobody chose.
	StreamMaxAge time.Duration

	// Consumer is the durable consumer name. Durable so a restart resumes rather than
	// replaying the stream from the beginning.
	Consumer string

	// MaxDeliver bounds redelivery of one message before the consumer dead-letters it.
	MaxDeliver int

	// AckWait is how long the server waits for an ack before redelivering. It must exceed
	// the handler's worst-case runtime, or a slow handler causes redelivery of work that is
	// still in progress -- which looks exactly like a duplicate bug.
	AckWait time.Duration
}

// UpstreamConfig configures calls this service makes TO other services.
//
// There is deliberately no address here. A platform package cannot know your upstreams, and a
// single UPSTREAM_ADDR would be wrong the moment there are two of them -- so the target is
// passed by whoever builds the connection, and only the cross-cutting behaviour lives here.
type UpstreamConfig struct {
	// DefaultTimeout bounds an outbound call whose context carries no deadline.
	//
	// Separate from the server's DEFAULT_TIMEOUT on purpose: that one decides how long this
	// service is willing to WORK, and this one how long it is willing to WAIT. They are
	// tuned by different evidence and there is no reason for them to move together.
	DefaultTimeout time.Duration

	// ReserveFraction is the share of the remaining deadline kept back for this service.
	//
	// It is what makes an upstream timeout land INSIDE your handler instead of alongside it.
	// Spend the caller's whole budget on the upstream call and, when the upstream is slow,
	// your handler is cancelled at the same instant -- no log line naming the culprit, no
	// metric, and a caller who learns only that something somewhere was slow.
	//
	// 0.1 leaves a tenth. Too small and there is no room to record anything; too large and
	// you fail calls that would have succeeded.
	ReserveFraction float64

	// MinBudget is the least remaining time worth making a call with.
	//
	// Below it the client fails immediately without dialling. Making the call anyway spends
	// a connection, a goroutine and a full upstream handler on an answer that is already
	// certain to arrive too late -- and the upstream has no way to know that, so it does the
	// entire job before discovering nobody is listening.
	MinBudget time.Duration
}

type ServerConfig struct {
	// MaxConcurrentStreams must be set: grpc-go's default is effectively unbounded, so
	// one client can open enough streams to exhaust memory.
	MaxConcurrentStreams uint32
	MaxRecvMsgBytes      int

	// MaxConnectionAge forces a periodic GOAWAY. Without it, long-lived HTTP/2
	// connections pin themselves to whichever pods existed when the client started, and
	// a rolling deploy never rebalances traffic.
	MaxConnectionAge      time.Duration
	MaxConnectionAgeGrace time.Duration
	KeepaliveMinTime      time.Duration

	// AdmissionLimit bounds concurrent in-flight RPCs. Sized from the DB pool, because
	// the pool is the real bottleneck -- admitting more work than you can execute just
	// converts a fast rejection into a slow timeout.
	AdmissionLimit int

	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
}

type ShutdownConfig struct {
	// DrainDelay is the gap between flipping health to NOT_SERVING and actually refusing
	// work. Kubernetes removes a pod from Service endpoints asynchronously, so a pod that
	// stops instantly on SIGTERM still receives requests for a short window. This is that
	// window. M10 will assert terminationGracePeriodSeconds exceeds DrainDelay +
	// GracePeriod, once the kustomize overlays exist.
	DrainDelay time.Duration

	// GracePeriod bounds GracefulStop before connections are cut.
	GracePeriod time.Duration
}

// Load reads the process environment for a service that serves traffic.
func Load() (Config, error) { return Parse(environ()) }

// LoadWorker reads the process environment for cmd/worker.
//
// See Role: the worker opens no listener, so the auth settings a server must get right are
// irrelevant to it, and refusing to start over them is a bug rather than a safeguard.
func LoadWorker() (Config, error) { return ParseFor(environ(), RoleWorker) }

// Parse builds a Config from an explicit map, for a traffic-serving process.
//
// Tests call this rather than t.Setenv. That is not a style preference: t.Setenv mutates
// process-global state, so Go refuses to run such a test in parallel, and a config
// package is exactly where you want a hundred cheap parallel table cases.
func Parse(env map[string]string) (Config, error) { return ParseFor(env, RoleServer) }

// ParseFor builds a Config for a specific role.
func ParseFor(env map[string]string, role Role) (Config, error) {
	p := &parser{env: env}

	cfg := Config{
		Role:        role,
		AppEnv:      p.str("APP_ENV", EnvDevelopment),
		ServiceName: p.str("SERVICE_NAME", "orderd"),
		Version:     p.str("VERSION", "dev"),

		GRPCAddr:    p.str("GRPC_ADDR", ":50051"),
		AdminAddr:   p.str("ADMIN_ADDR", "127.0.0.1:9090"),
		GatewayAddr: p.str("GATEWAY_ADDR", ":8080"),

		LogLevel:  p.str("LOG_LEVEL", "info"),
		LogFormat: p.str("LOG_FORMAT", "json"),

		StoreDriver: p.str("STORE_DRIVER", StoreMemory),
		AuthMode:    p.str("AUTH_MODE", AuthDev),

		Postgres: PostgresConfig{
			DSN:             Secret(p.str("POSTGRES_DSN", "")),
			MaxOpenConns:    p.intVal("POSTGRES_MAX_OPEN_CONNS", 10),
			MaxIdleConns:    p.intVal("POSTGRES_MAX_IDLE_CONNS", 5),
			ConnMaxLifetime: p.dur("POSTGRES_CONN_MAX_LIFETIME", 30*time.Minute),
			ConnMaxIdleTime: p.dur("POSTGRES_CONN_MAX_IDLE_TIME", 5*time.Minute),
		},

		OIDC: OIDCConfig{
			IssuerURL:    p.str("OIDC_ISSUER_URL", ""),
			Audience:     p.str("OIDC_AUDIENCE", ""),
			JWKSURL:      p.str("OIDC_JWKS_URL", ""),
			Leeway:       p.dur("OIDC_LEEWAY", 30*time.Second),
			TenantClaim:  p.str("OIDC_TENANT_CLAIM", "tenant_id"),
			ScopeClaim:   p.str("OIDC_SCOPE_CLAIM", "scope"),
			ServiceClaim: p.str("OIDC_SERVICE_CLAIM", "token_use"),
			MaxKeyAge:    p.dur("OIDC_MAX_KEY_AGE", 15*time.Minute),
		},

		Redis: RedisConfig{
			Addr:               p.str("REDIS_ADDR", ""),
			Password:           Secret(p.str("REDIS_PASSWORD", "")),
			DB:                 p.intVal("REDIS_DB", 0),
			RateLimitPerMinute: p.intVal("RATE_LIMIT_PER_MINUTE", 600),
			RateLimitBurst:     p.intVal("RATE_LIMIT_BURST", 100),
		},

		Outbox: OutboxConfig{
			BatchSize:       p.intVal("OUTBOX_BATCH_SIZE", 100),
			PollInterval:    p.dur("OUTBOX_POLL_INTERVAL", time.Second),
			MaxConns:        p.intVal("OUTBOX_MAX_CONNS", 2),
			ObserveInterval: p.dur("OUTBOX_OBSERVE_INTERVAL", 15*time.Second),
		},

		// The default ProcessedEvents (8 days) deliberately exceeds the default StreamMaxAge
		// (7 days) by a full day rather than by an hour. The margin absorbs clock skew between
		// the broker and the database, and a prune job that does not run on schedule.
		Retention: RetentionConfig{
			Outbox:          p.dur("RETENTION_OUTBOX", 168*time.Hour),
			ProcessedEvents: p.dur("RETENTION_PROCESSED_EVENTS", 192*time.Hour),
			BatchSize:       p.intVal("RETENTION_BATCH_SIZE", 1000),
		},

		NATS: NATSConfig{
			URL:              p.str("NATS_URL", ""),
			Stream:           p.str("NATS_STREAM", "EVENTS"),
			SubjectPrefix:    p.str("NATS_SUBJECT_PREFIX", "events"),
			DLQSubjectPrefix: p.str("NATS_DLQ_SUBJECT_PREFIX", "dlq"),
			DuplicateWindow:  p.dur("NATS_DUPLICATE_WINDOW", 2*time.Minute),
			StreamMaxAge:     p.dur("NATS_STREAM_MAX_AGE", 168*time.Hour),
			Consumer:         p.str("NATS_CONSUMER", "order-projection"),
			MaxDeliver:       p.intVal("NATS_MAX_DELIVER", 5),
			AckWait:          p.dur("NATS_ACK_WAIT", 30*time.Second),
		},

		Upstream: UpstreamConfig{
			DefaultTimeout:  p.dur("UPSTREAM_DEFAULT_TIMEOUT", 10*time.Second),
			ReserveFraction: p.float("UPSTREAM_RESERVE_FRACTION", 0.1),
			MinBudget:       p.dur("UPSTREAM_MIN_BUDGET", 50*time.Millisecond),
		},

		Telemetry: TelemetryConfig{
			OTLPEndpoint:     p.str("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
			TraceSampleRatio: p.float("OTEL_TRACE_SAMPLE_RATIO", 1.0),
		},

		Server: ServerConfig{
			MaxConcurrentStreams:  uint32(p.intVal("GRPC_MAX_CONCURRENT_STREAMS", 250)),
			MaxRecvMsgBytes:       p.intVal("GRPC_MAX_RECV_MSG_BYTES", 4<<20),
			MaxConnectionAge:      p.dur("GRPC_MAX_CONNECTION_AGE", 30*time.Minute),
			MaxConnectionAgeGrace: p.dur("GRPC_MAX_CONNECTION_AGE_GRACE", 30*time.Second),
			KeepaliveMinTime:      p.dur("GRPC_KEEPALIVE_MIN_TIME", 30*time.Second),
			AdmissionLimit:        p.intVal("ADMISSION_LIMIT", 0), // 0 => derive from the pool
			DefaultTimeout:        p.dur("DEFAULT_TIMEOUT", 15*time.Second),
			MaxTimeout:            p.dur("MAX_TIMEOUT", 60*time.Second),
		},

		Shutdown: ShutdownConfig{
			DrainDelay:  p.dur("SHUTDOWN_DRAIN_DELAY", 5*time.Second),
			GracePeriod: p.dur("SHUTDOWN_GRACE_PERIOD", 25*time.Second),
		},
	}

	if len(p.errs) > 0 {
		return Config{}, errors.Join(p.errs...)
	}
	if cfg.Server.AdmissionLimit == 0 {
		cfg.Server.AdmissionLimit = cfg.Postgres.MaxOpenConns * 4
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate fails fast, and reports EVERY problem rather than only the first.
//
// Reporting one at a time turns a misconfigured deploy into five rollout attempts.
func (c Config) Validate() error {
	var errs []error

	if !oneOf(c.AppEnv, EnvDevelopment, EnvStaging, EnvProduction) {
		errs = append(errs, fmt.Errorf("APP_ENV %q must be one of development, staging, production", c.AppEnv))
	}
	if !oneOf(c.StoreDriver, StoreMemory, StorePostgres) {
		errs = append(errs, fmt.Errorf("STORE_DRIVER %q must be one of memory, postgres", c.StoreDriver))
	}
	if !oneOf(c.AuthMode, AuthDev, AuthOIDC) {
		errs = append(errs, fmt.Errorf("AUTH_MODE %q must be one of dev, oidc", c.AuthMode))
	}

	// The guard that matters most in this file.
	//
	// AUTH_MODE=dev accepts an unsigned, unexpiring token. It exists so a fresh clone
	// runs with no identity provider. Shipping it to production would be a total
	// authentication bypass, so it is refused before any listener opens rather than
	// left to a code review.
	//
	// Scoped to RoleServer. cmd/worker opens no listener and verifies no tokens, so there is
	// no unauthenticated traffic for this to prevent -- and applying it there stopped the
	// worker booting in production at all. See the Role type for how that was found.
	if c.Role == RoleServer && c.AppEnv == EnvProduction && c.AuthMode == AuthDev {
		errs = append(errs, errors.New(
			"AUTH_MODE=dev is refused when APP_ENV=production: the dev verifier accepts any token"))
	}

	// The worker's own hard requirement, checked here rather than in main so it joins every
	// other misconfiguration in a single startup error instead of arriving on its own.
	if c.Role == RoleWorker && c.Postgres.DSN.Reveal() == "" {
		errs = append(errs, errors.New("POSTGRES_DSN is required: the outbox lives in the database"))
	}

	if c.StoreDriver == StorePostgres && c.Postgres.DSN.Reveal() == "" {
		errs = append(errs, errors.New("POSTGRES_DSN is required when STORE_DRIVER=postgres"))
	}

	// THE RETENTION FLOOR, and the reason it is refused here rather than documented.
	//
	// processed_events is the consumer's memory of what it has already applied. A row may be
	// deleted only once the broker can no longer deliver that message -- which is when the
	// stream's own retention has dropped it. Set this below NATS_STREAM_MAX_AGE and there is a
	// window in which a message still exists on the broker while the record of having handled
	// it does not: a redelivery in that window is applied a second time.
	//
	// What makes it worth failing startup over is that nothing downstream would report it. The
	// projection increments a counter twice, no error is logged, no metric moves, and the wrong
	// number is still there tomorrow. It would be found, if ever, by someone reconciling
	// figures weeks later -- at which point the events that caused it are long gone from the
	// stream and the cause is unknowable.
	//
	// Unconditional rather than scoped to a role: this is a coherence check between two values,
	// and both are read from the same environment whichever binary is starting.
	if c.Retention.ProcessedEvents > 0 && c.Retention.ProcessedEvents <= c.NATS.StreamMaxAge {
		errs = append(errs, fmt.Errorf(
			"RETENTION_PROCESSED_EVENTS (%s) must be GREATER than NATS_STREAM_MAX_AGE (%s): "+
				"a deduplication row deleted while the broker can still redeliver its message "+
				"lets the consumer apply that event twice, silently and permanently",
			c.Retention.ProcessedEvents, c.NATS.StreamMaxAge))
	}
	if c.Retention.Outbox <= 0 {
		errs = append(errs, fmt.Errorf("RETENTION_OUTBOX (%s) must be positive", c.Retention.Outbox))
	}
	if c.Retention.ProcessedEvents <= 0 {
		errs = append(errs, fmt.Errorf("RETENTION_PROCESSED_EVENTS (%s) must be positive",
			c.Retention.ProcessedEvents))
	}
	if c.Retention.BatchSize <= 0 {
		errs = append(errs, fmt.Errorf("RETENTION_BATCH_SIZE (%d) must be positive; an unbounded "+
			"DELETE holds a lock for its whole duration", c.Retention.BatchSize))
	}

	// AUTH_MODE=oidc requires enough configuration to actually verify something.
	//
	// This block replaces a blanket refusal of AUTH_MODE=oidc that stood until the verifier
	// existed, and the history is worth keeping because it explains the shape. The server
	// once installed the dev interceptor unconditionally and never read AuthMode, so
	// APP_ENV=production AUTH_MODE=oidc validated, booted with no warning, and served every
	// request as a full-scope dev-tenant principal -- a total bypass that sprang only for
	// the reader who believed the documentation and set the safe-looking value.
	//
	// The lesson carried forward: `oidc` must never be a value that LOOKS configured while
	// verifying nothing. Half-configured is the modern version of that failure -- an issuer
	// with no audience accepts every token the IdP ever minted, including tokens for other
	// applications entirely -- so it is refused here, before any listener opens, as well as
	// in NewOIDCVerifier.
	if c.AuthMode == AuthOIDC {
		if c.OIDC.IssuerURL == "" {
			errs = append(errs, errors.New("OIDC_ISSUER_URL is required when AUTH_MODE=oidc"))
		}
		if c.OIDC.Audience == "" {
			errs = append(errs, errors.New(
				"OIDC_AUDIENCE is required when AUTH_MODE=oidc: without it this service accepts "+
					"any token the issuer signed, including tokens issued to other applications"))
		}
		// TrimSpace, not != "": OIDC_TENANT_CLAIM=" " survives the parser's empty-means-
		// default rule and would otherwise reach the verifier as a claim path made of one
		// space, which matches nothing and denies everyone for a reason nobody can see.
		if strings.TrimSpace(c.OIDC.TenantClaim) == "" {
			errs = append(errs, errors.New(
				"OIDC_TENANT_CLAIM must not be empty: the tenant comes from the verified token, "+
					"never from the request body"))
		}
	}

	// Only validated when Redis is actually configured. A service running without it should
	// not have to supply coherent quota numbers for a limiter it never builds.
	if c.Redis.Addr != "" {
		if c.Redis.RateLimitPerMinute <= 0 {
			errs = append(errs, fmt.Errorf("RATE_LIMIT_PER_MINUTE must be positive when REDIS_ADDR is set, got %d",
				c.Redis.RateLimitPerMinute))
		}
		if c.Redis.RateLimitBurst <= 0 {
			// Burst zero admits NOTHING: the GCRA tolerance becomes zero, so even the first
			// request from an idle key arrives before its own theoretical arrival time. A
			// limiter that rejects 100% of traffic is worth refusing at startup.
			errs = append(errs, fmt.Errorf("RATE_LIMIT_BURST must be positive when REDIS_ADDR is set, got %d",
				c.Redis.RateLimitBurst))
		}
	}

	if c.Outbox.BatchSize <= 0 {
		errs = append(errs, fmt.Errorf("OUTBOX_BATCH_SIZE must be positive, got %d", c.Outbox.BatchSize))
	}
	if c.Outbox.MaxConns <= 0 {
		errs = append(errs, fmt.Errorf("OUTBOX_MAX_CONNS must be positive, got %d", c.Outbox.MaxConns))
	}

	// Only validated when a broker is actually configured. NATS_URL empty means cmd/worker
	// uses outbox.LogPublisher, and a clone with no broker should not have to supply coherent
	// stream settings for a stream it never creates.
	errs = append(errs, c.validateNATS()...)

	// A reserve outside (0,1) is not a tuning choice, it is a broken one: at 0 the upstream
	// gets the entire deadline and the headroom this exists for is gone, and at 1 or above
	// every call is refused before it is made.
	if c.Upstream.ReserveFraction <= 0 || c.Upstream.ReserveFraction >= 1 {
		errs = append(errs, fmt.Errorf(
			"UPSTREAM_RESERVE_FRACTION %v must be between 0 and 1 exclusive: at 0 an upstream "+
				"call consumes the caller's whole deadline and your handler is cancelled with it",
			c.Upstream.ReserveFraction))
	}
	if c.Upstream.DefaultTimeout <= 0 {
		errs = append(errs, fmt.Errorf("UPSTREAM_DEFAULT_TIMEOUT must be positive, got %s",
			c.Upstream.DefaultTimeout))
	}
	if c.Upstream.MinBudget <= 0 {
		errs = append(errs, fmt.Errorf("UPSTREAM_MIN_BUDGET must be positive, got %s",
			c.Upstream.MinBudget))
	}
	if c.Upstream.MinBudget >= c.Upstream.DefaultTimeout {
		// Nothing would ever be callable: a call with no deadline gets DefaultTimeout, which
		// would immediately be judged too small to bother making.
		errs = append(errs, fmt.Errorf(
			"UPSTREAM_MIN_BUDGET (%s) must be less than UPSTREAM_DEFAULT_TIMEOUT (%s), or every "+
				"call with no deadline is refused before it is made",
			c.Upstream.MinBudget, c.Upstream.DefaultTimeout))
	}

	if c.Postgres.MaxOpenConns <= 0 {
		errs = append(errs, errors.New("POSTGRES_MAX_OPEN_CONNS must be positive and explicit"))
	}
	if c.Server.MaxTimeout < c.Server.DefaultTimeout {
		errs = append(errs, fmt.Errorf("MAX_TIMEOUT (%s) must be >= DEFAULT_TIMEOUT (%s)",
			c.Server.MaxTimeout, c.Server.DefaultTimeout))
	}
	if c.Telemetry.TraceSampleRatio < 0 || c.Telemetry.TraceSampleRatio > 1 {
		errs = append(errs, fmt.Errorf("OTEL_TRACE_SAMPLE_RATIO %v must be between 0 and 1",
			c.Telemetry.TraceSampleRatio))
	}
	if !oneOf(c.LogFormat, "json", "text") {
		errs = append(errs, fmt.Errorf("LOG_FORMAT %q must be json or text", c.LogFormat))
	}
	if _, err := parseLevel(c.LogLevel); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// validateNATS checks the broker settings, including two cross-field constraints that are
// invisible when each value is read on its own.
//
// The subject checks here cover OPERATOR-supplied settings, which are fixed for the life of
// the process and so belong in a startup failure. internal/platform/events validates the
// event type separately, because that comes from DATA and can only be judged per message.
// The two are not duplicates of each other; they fail at different times, for different
// reasons, and only one of them can refuse to boot.
func (c Config) validateNATS() []error {
	if c.NATS.URL == "" {
		return nil
	}

	var errs []error

	for _, f := range []struct{ name, value string }{
		{"NATS_STREAM", c.NATS.Stream},
		{"NATS_SUBJECT_PREFIX", c.NATS.SubjectPrefix},
		{"NATS_DLQ_SUBJECT_PREFIX", c.NATS.DLQSubjectPrefix},
		{"NATS_CONSUMER", c.NATS.Consumer},
	} {
		if strings.TrimSpace(f.value) == "" {
			errs = append(errs, fmt.Errorf("%s must not be empty when NATS_URL is set", f.name))
			continue
		}
		// Wildcards and whitespace in a name or prefix are not a style problem. A prefix of
		// "*" makes every publish land on a subject that a single-token filter matches, and
		// whitespace is rejected by the NATS client at publish time -- as a per-message
		// error, forever, on a setting that could have failed here at startup instead.
		if strings.ContainsAny(f.value, "*> \t\r\n") {
			errs = append(errs, fmt.Errorf("%s %q must not contain a wildcard (* or >) or whitespace",
				f.name, f.value))
		}
	}

	// THE DEAD-LETTER LOOP.
	//
	// The consumer filters {NATS_SUBJECT_PREFIX}.>, and dead-letters to
	// {NATS_DLQ_SUBJECT_PREFIX}.{original subject}. If the DLQ prefix sits inside the
	// consumer's own filter, every dead letter is redelivered to the consumer that just gave
	// up on it, dead-lettered again, and the stream fills with an exponentially growing chain
	// of failures at the speed of the network.
	//
	// Nothing about either value looks wrong in isolation, which is why this is checked here
	// rather than left to be discovered under load.
	if prefixed(c.NATS.DLQSubjectPrefix, c.NATS.SubjectPrefix) {
		errs = append(errs, fmt.Errorf(
			"NATS_DLQ_SUBJECT_PREFIX %q is inside NATS_SUBJECT_PREFIX %q: the consumer would "+
				"redeliver its own dead letters forever", c.NATS.DLQSubjectPrefix, c.NATS.SubjectPrefix))
	}

	if c.NATS.DuplicateWindow <= 0 {
		errs = append(errs, fmt.Errorf("NATS_DUPLICATE_WINDOW must be positive, got %s", c.NATS.DuplicateWindow))
	} else if c.NATS.DuplicateWindow <= c.Outbox.PollInterval {
		// The window has to outlive the relay's retry gap.
		//
		// A batch whose marking transaction fails is republished on the NEXT drain -- one
		// poll interval later. If JetStream has already forgotten the message id by then, the
		// republish is stored as a second message and every consumer sees the event twice.
		// The defaults (2m vs 1s) are nowhere near this line; a fork tuning either value can
		// cross it without touching the other.
		errs = append(errs, fmt.Errorf(
			"NATS_DUPLICATE_WINDOW (%s) must exceed OUTBOX_POLL_INTERVAL (%s): a republished "+
				"batch arriving after the window is stored again instead of deduplicated",
			c.NATS.DuplicateWindow, c.Outbox.PollInterval))
	}

	if c.NATS.StreamMaxAge <= 0 {
		errs = append(errs, fmt.Errorf("NATS_STREAM_MAX_AGE must be positive, got %s", c.NATS.StreamMaxAge))
	}
	if c.NATS.MaxDeliver <= 0 {
		errs = append(errs, fmt.Errorf("NATS_MAX_DELIVER must be positive, got %d", c.NATS.MaxDeliver))
	}
	if c.NATS.AckWait <= 0 {
		errs = append(errs, fmt.Errorf("NATS_ACK_WAIT must be positive, got %s", c.NATS.AckWait))
	}

	return errs
}

// prefixed reports whether subject sits inside prefix's subject subtree.
//
// Token-aware on purpose: "eventsx" is NOT inside "events", but "events.dlq" is. A plain
// strings.HasPrefix would get the first case wrong and reject a perfectly valid pair.
func prefixed(subject, prefix string) bool {
	return subject == prefix || strings.HasPrefix(subject, prefix+".")
}

// IsProduction reports whether this is a production deployment.
func (c Config) IsProduction() bool { return c.AppEnv == EnvProduction }

type parser struct {
	env  map[string]string
	errs []error
}

func (p *parser) str(key, def string) string {
	if v, ok := p.env[key]; ok && v != "" {
		return v
	}
	return def
}

func (p *parser) intVal(key string, def int) int {
	raw, ok := p.env[key]
	if !ok || raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: %q is not an integer", key, raw))
		return def
	}
	return n
}

func (p *parser) float(key string, def float64) float64 {
	raw, ok := p.env[key]
	if !ok || raw == "" {
		return def
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: %q is not a number", key, raw))
		return def
	}
	return f
}

func (p *parser) dur(key string, def time.Duration) time.Duration {
	raw, ok := p.env[key]
	if !ok || raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: %q is not a duration (try 30s, 5m)", key, raw))
		return def
	}
	return d
}

func environ() map[string]string {
	out := make(map[string]string)
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func parseLevel(s string) (int, error) {
	switch strings.ToLower(s) {
	case "debug":
		return -4, nil
	case "info":
		return 0, nil
	case "warn", "warning":
		return 4, nil
	case "error":
		return 8, nil
	default:
		return 0, fmt.Errorf("LOG_LEVEL %q must be one of debug, info, warn, error", s)
	}
}
