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

// Secret wraps a value that must never reach a log, an error string, or a crash dump.
//
// It implements Stringer, json.Marshaler and slog.LogValuer, which are the three routes
// by which a password normally escapes: fmt.Printf("%+v", cfg), a JSON dump of config,
// and structured logging. Covering only one of the three is the usual mistake.
type Secret string

func (s Secret) String() string                { return "[REDACTED]" }
func (s Secret) GoString() string              { return "[REDACTED]" }
func (s Secret) MarshalJSON() ([]byte, error)  { return []byte(`"[REDACTED]"`), nil }
func (s Secret) LogValue() any                 { return "[REDACTED]" }
func (s Secret) Reveal() string                { return string(s) }

// Config is the full runtime configuration.
type Config struct {
	AppEnv      string
	ServiceName string
	Version     string

	// Three listeners, deliberately.
	//
	// GRPCAddr and GatewayAddr are public. AdminAddr is NOT: it carries /metrics and
	// /debug/pprof, and net/http/pprof registers itself on http.DefaultServeMux in an
	// init() function. Serving that on the ingress port hands anyone a heap dumper and a
	// CPU-profiler trigger. observability/admin_test.go asserts the split.
	GRPCAddr    string
	GatewayAddr string
	AdminAddr   string

	LogLevel  string
	LogFormat string

	StoreDriver string
	Postgres    PostgresConfig

	AuthMode string
	OIDC     OIDCConfig

	Redis RedisConfig
	NATS  NATSConfig

	Server   ServerConfig
	Shutdown ShutdownConfig
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
	JWKSURL   string
	// Leeway tolerates clock skew between this service and the identity provider.
	Leeway time.Duration
}

type RedisConfig struct {
	Addr     string
	Password Secret
	DB       int

	CacheTTL       time.Duration
	CacheTTLJitter time.Duration

	RateLimitPerMinute int
	RateLimitBurst     int
}

type NATSConfig struct {
	URL        string
	StreamName string
	Subject    string

	// Durable names the JetStream consumer. It must be stable across restarts, otherwise
	// every deploy creates a fresh consumer and replays the whole stream.
	Durable string
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
	// window. deploy/k8s/kustomize_test.go asserts terminationGracePeriodSeconds exceeds
	// DrainDelay + GracePeriod.
	DrainDelay time.Duration

	// GracePeriod bounds GracefulStop before connections are cut.
	GracePeriod time.Duration
}

// Load reads the process environment.
func Load() (Config, error) { return Parse(environ()) }

// Parse builds a Config from an explicit map.
//
// Tests call this rather than t.Setenv. That is not a style preference: t.Setenv mutates
// process-global state, so Go refuses to run such a test in parallel, and a config
// package is exactly where you want a hundred cheap parallel table cases.
func Parse(env map[string]string) (Config, error) {
	p := &parser{env: env}

	cfg := Config{
		AppEnv:      p.str("APP_ENV", EnvDevelopment),
		ServiceName: p.str("SERVICE_NAME", "orderd"),
		Version:     p.str("VERSION", "dev"),

		GRPCAddr:    p.str("GRPC_ADDR", ":50051"),
		GatewayAddr: p.str("GATEWAY_ADDR", ":8080"),
		AdminAddr:   p.str("ADMIN_ADDR", "127.0.0.1:9090"),

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
			IssuerURL: p.str("OIDC_ISSUER_URL", ""),
			Audience:  p.str("OIDC_AUDIENCE", ""),
			JWKSURL:   p.str("OIDC_JWKS_URL", ""),
			Leeway:    p.dur("OIDC_LEEWAY", 30*time.Second),
		},

		Redis: RedisConfig{
			Addr:               p.str("REDIS_ADDR", ""),
			Password:           Secret(p.str("REDIS_PASSWORD", "")),
			DB:                 p.intVal("REDIS_DB", 0),
			CacheTTL:           p.dur("REDIS_CACHE_TTL", time.Minute),
			CacheTTLJitter:     p.dur("REDIS_CACHE_TTL_JITTER", 10*time.Second),
			RateLimitPerMinute: p.intVal("RATE_LIMIT_PER_MINUTE", 600),
			RateLimitBurst:     p.intVal("RATE_LIMIT_BURST", 60),
		},

		NATS: NATSConfig{
			URL:        p.str("NATS_URL", ""),
			StreamName: p.str("NATS_STREAM", "ORDERS"),
			Subject:    p.str("NATS_SUBJECT", "orders.events"),
			Durable:    p.str("NATS_DURABLE", "orders-worker"),
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
	if c.AppEnv == EnvProduction && c.AuthMode == AuthDev {
		errs = append(errs, errors.New(
			"AUTH_MODE=dev is refused when APP_ENV=production: the dev verifier accepts any token"))
	}

	if c.StoreDriver == StorePostgres && c.Postgres.DSN.Reveal() == "" {
		errs = append(errs, errors.New("POSTGRES_DSN is required when STORE_DRIVER=postgres"))
	}
	if c.AuthMode == AuthOIDC {
		if c.OIDC.IssuerURL == "" {
			errs = append(errs, errors.New("OIDC_ISSUER_URL is required when AUTH_MODE=oidc"))
		}
		if c.OIDC.Audience == "" {
			errs = append(errs, errors.New("OIDC_AUDIENCE is required when AUTH_MODE=oidc"))
		}
	}

	if c.Postgres.MaxOpenConns <= 0 {
		errs = append(errs, errors.New("POSTGRES_MAX_OPEN_CONNS must be positive and explicit"))
	}
	if c.Server.MaxTimeout < c.Server.DefaultTimeout {
		errs = append(errs, fmt.Errorf("MAX_TIMEOUT (%s) must be >= DEFAULT_TIMEOUT (%s)",
			c.Server.MaxTimeout, c.Server.DefaultTimeout))
	}
	if !oneOf(c.LogFormat, "json", "text") {
		errs = append(errs, fmt.Errorf("LOG_FORMAT %q must be json or text", c.LogFormat))
	}
	if _, err := parseLevel(c.LogLevel); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
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
