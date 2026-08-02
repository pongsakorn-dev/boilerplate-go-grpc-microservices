package config_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/platform/config"
)

// Every test here calls config.Parse with an explicit map instead of t.Setenv.
//
// That is not a style preference. t.Setenv mutates process-global state, so Go refuses to
// run any test that uses it in parallel -- and a config package is exactly where you want a
// hundred cheap parallel table cases. Taking the environment as a parameter makes the whole
// package parallel-safe by construction.

func TestParseDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse(map[string]string{})
	if err != nil {
		t.Fatalf("an empty environment must produce a valid dev config, got: %v", err)
	}

	// These defaults are what make `git clone && go run ./cmd/orderd` work with no setup.
	if cfg.StoreDriver != config.StoreMemory {
		t.Errorf("STORE_DRIVER default = %q, want memory", cfg.StoreDriver)
	}
	if cfg.AuthMode != config.AuthDev {
		t.Errorf("AUTH_MODE default = %q, want dev", cfg.AuthMode)
	}
	if cfg.AppEnv != config.EnvDevelopment {
		t.Errorf("APP_ENV default = %q, want development", cfg.AppEnv)
	}
}

// TestValidateRefusesHalfConfiguredOIDC is the successor to a test that refused
// AUTH_MODE=oidc outright, and it guards the same bug in its surviving form.
//
// The original bug: the server installed the dev interceptor unconditionally and never read
// AuthMode, so APP_ENV=production AUTH_MODE=oidc validated, booted with no warning, and
// served every request as a full-scope dev-tenant principal -- and `oidc` is precisely the
// value a reader sets BECAUSE they read the warning about dev mode.
//
// The verifier now exists, so refusing `oidc` outright would be wrong. What must not come
// back is the shape: a configuration that LOOKS authenticated and verifies nothing. Each
// case below is one of those.
func TestValidateRefusesHalfConfiguredOIDC(t *testing.T) {
	t.Parallel()

	base := map[string]string{
		"APP_ENV":         config.EnvProduction,
		"AUTH_MODE":       config.AuthOIDC,
		"OIDC_ISSUER_URL": "https://issuer.example.com",
		"OIDC_AUDIENCE":   "orders",
	}

	cases := []struct {
		name    string
		mutate  func(map[string]string)
		wantHas string
	}{
		{
			// The one that actually bites. A verifier with no expected audience accepts
			// every token the issuer ever signed -- including tokens minted for a
			// completely different application that happens to share the IdP. It is a
			// breach, and it produces no error anywhere until someone notices the wrong
			// service's users have accounts.
			name:    "no audience accepts tokens meant for other applications",
			mutate:  func(m map[string]string) { delete(m, "OIDC_AUDIENCE") },
			wantHas: "OIDC_AUDIENCE",
		},
		{
			name:    "no issuer means nothing to discover keys from",
			mutate:  func(m map[string]string) { delete(m, "OIDC_ISSUER_URL") },
			wantHas: "OIDC_ISSUER_URL",
		},
		{
			// An empty tenant claim would authenticate callers into no tenant at all,
			// which the store layer must then interpret -- and "no tenant" is one bad
			// branch away from "every tenant".
			name:    "empty tenant claim breaks the tenant-comes-from-the-token rule",
			mutate:  func(m map[string]string) { m["OIDC_TENANT_CLAIM"] = " " },
			wantHas: "OIDC_TENANT_CLAIM",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := make(map[string]string, len(base))
			for k, v := range base {
				env[k] = v
			}
			tc.mutate(env)

			_, err := config.Parse(env)
			if err == nil {
				t.Fatalf("this configuration was accepted, but it verifies nothing meaningful: %v", env)
			}
			if !strings.Contains(err.Error(), tc.wantHas) {
				t.Errorf("error does not name %s: %v", tc.wantHas, err)
			}
		})
	}
}

// TestValidateAcceptsFullyConfiguredOIDC is the other half, and the one that would have
// caught a refusal left in place after the verifier landed.
func TestValidateAcceptsFullyConfiguredOIDC(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse(map[string]string{
		"APP_ENV":         config.EnvProduction,
		"AUTH_MODE":       config.AuthOIDC,
		"OIDC_ISSUER_URL": "https://issuer.example.com/realms/gomicro",
		"OIDC_AUDIENCE":   "orderd",
	})
	if err != nil {
		t.Fatalf("a fully configured OIDC deployment was refused: %v", err)
	}

	// The claim defaults are what let a Keycloak deployment set two variables instead of
	// four. If they ever become empty, NewOIDCVerifier refuses to build -- so assert them
	// here, where the failure names the cause.
	if cfg.OIDC.TenantClaim != "tenant_id" {
		t.Errorf("OIDC_TENANT_CLAIM default = %q, want tenant_id", cfg.OIDC.TenantClaim)
	}
	if cfg.OIDC.ScopeClaim != "scope" {
		t.Errorf("OIDC_SCOPE_CLAIM default = %q, want scope", cfg.OIDC.ScopeClaim)
	}
}

// TestValidateRejectsDevAuthInProduction is the single most important assertion in this
// package.
//
// AUTH_MODE=dev accepts every request without verifying anything. It exists so a fresh
// clone runs with no identity provider. Shipping it to production is a total authentication
// bypass -- so it is refused before any listener opens, rather than left to code review.
func TestValidateRejectsDevAuthInProduction(t *testing.T) {
	t.Parallel()

	_, err := config.Parse(map[string]string{
		"APP_ENV":   config.EnvProduction,
		"AUTH_MODE": config.AuthDev,
	})

	if err == nil {
		t.Fatal("AUTH_MODE=dev was accepted with APP_ENV=production -- that is an " +
			"authentication bypass in production")
	}
	if !strings.Contains(err.Error(), "AUTH_MODE=dev") {
		t.Errorf("the error does not name the problem: %v", err)
	}
}

// TestValidateReportsEveryProblemAtOnce turns a five-attempt rollout into one.
func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()

	_, err := config.Parse(map[string]string{
		"APP_ENV":      "prod",   // invalid: must be "production"
		"STORE_DRIVER": "mysql",  // invalid
		"AUTH_MODE":    "magic",  // invalid
		"LOG_FORMAT":   "logfmt", // invalid
	})
	if err == nil {
		t.Fatal("expected validation errors")
	}

	msg := err.Error()
	for _, want := range []string{"APP_ENV", "STORE_DRIVER", "AUTH_MODE", "LOG_FORMAT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %s; reporting one problem at a time turns "+
				"a misconfigured deploy into several rollout attempts.\ngot: %v", want, err)
		}
	}
}

func TestValidateRequiresDependenciesForTheChosenMode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "postgres driver without a DSN",
			env:  map[string]string{"STORE_DRIVER": config.StorePostgres},
			want: "POSTGRES_DSN",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Parse(tc.env)
			if err == nil {
				t.Fatalf("expected an error naming %s", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

func TestParseRejectsMalformedValues(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, key, value string }{
		{"non-integer pool size", "POSTGRES_MAX_OPEN_CONNS", "lots"},
		{"non-duration timeout", "DEFAULT_TIMEOUT", "quickly"},
		{"non-numeric sample ratio", "OTEL_TRACE_SAMPLE_RATIO", "half"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Parse(map[string]string{tc.key: tc.value})
			if err == nil {
				t.Fatalf("%s=%q was accepted", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("the error does not name %s: %v", tc.key, err)
			}
		})
	}
}

func TestSampleRatioMustBeAProbability(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"-0.5", "1.5", "42"} {
		if _, err := config.Parse(map[string]string{"OTEL_TRACE_SAMPLE_RATIO": v}); err == nil {
			t.Errorf("OTEL_TRACE_SAMPLE_RATIO=%s was accepted; it must be between 0 and 1", v)
		}
	}
}

// TestSecretIsRedactedThroughEveryEscapeRoute covers all three ways a password normally
// leaks. Covering only one is the usual mistake, and the one you miss is the one that ends
// up in the log aggregator.
func TestSecretIsRedactedThroughEveryEscapeRoute(t *testing.T) {
	t.Parallel()

	const password = "hunter2-super-secret"
	secret := config.Secret(password)

	t.Run("fmt verbs", func(t *testing.T) {
		t.Parallel()
		for _, format := range []string{"%v", "%s", "%+v", "%#v", "%q"} {
			out := fmt.Sprintf(format, secret)
			if strings.Contains(out, password) {
				t.Errorf("fmt.Sprintf(%q, secret) leaked the value: %s", format, out)
			}
		}
	})

	t.Run("struct dump", func(t *testing.T) {
		t.Parallel()
		// The realistic leak: someone logs the whole config during startup debugging.
		cfg, err := config.Parse(map[string]string{
			"STORE_DRIVER": config.StorePostgres,
			"POSTGRES_DSN": "postgres://user:" + password + "@db:5432/orders",
		})
		if err != nil {
			t.Fatalf("config.Parse: %v", err)
		}
		if out := fmt.Sprintf("%+v", cfg); strings.Contains(out, password) {
			t.Errorf("dumping the whole config leaked the DSN password")
		}
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()
		b, err := json.Marshal(map[string]config.Secret{"dsn": secret})
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if strings.Contains(string(b), password) {
			t.Errorf("JSON encoding leaked the value: %s", b)
		}
	})

	t.Run("structured logging", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("config", slog.Any("dsn", secret))
		if strings.Contains(buf.String(), password) {
			t.Errorf("slog leaked the value: %s", buf.String())
		}
	})

	t.Run("Reveal is the only way out", func(t *testing.T) {
		t.Parallel()
		if secret.Reveal() != password {
			t.Error("Reveal() must return the real value; it is how the driver gets the DSN")
		}
	})
}

// TestAdmissionLimitIsDerivedFromThePool documents the sizing relationship.
//
// The database pool is the real bottleneck. Admitting far more concurrent work than there
// are connections does not increase throughput -- it converts fast rejections into slow
// timeouts while the queued requests hold memory.
func TestAdmissionLimitIsDerivedFromThePool(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse(map[string]string{"POSTGRES_MAX_OPEN_CONNS": "25"})
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	if cfg.Server.AdmissionLimit <= cfg.Postgres.MaxOpenConns {
		t.Errorf("AdmissionLimit %d should exceed the pool size %d (some requests never "+
			"touch the database)", cfg.Server.AdmissionLimit, cfg.Postgres.MaxOpenConns)
	}

	explicit, err := config.Parse(map[string]string{"ADMISSION_LIMIT": "7"})
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	if explicit.Server.AdmissionLimit != 7 {
		t.Errorf("an explicit ADMISSION_LIMIT was overridden: got %d, want 7",
			explicit.Server.AdmissionLimit)
	}
}

// TestTimeoutOrderingIsValidated catches a config that would clamp every request to less
// than the default it advertises.
func TestTimeoutOrderingIsValidated(t *testing.T) {
	t.Parallel()

	_, err := config.Parse(map[string]string{
		"DEFAULT_TIMEOUT": "60s",
		"MAX_TIMEOUT":     "10s",
	})
	if err == nil {
		t.Fatal("MAX_TIMEOUT below DEFAULT_TIMEOUT was accepted")
	}
}

// TestNATSSettingsAreOnlyValidatedWhenABrokerIsConfigured keeps the no-broker path free.
//
// NATS_URL empty means cmd/worker uses outbox.LogPublisher, and a clone with no broker must
// not have to supply coherent stream settings for a stream it never creates.
func TestNATSSettingsAreOnlyValidatedWhenABrokerIsConfigured(t *testing.T) {
	t.Parallel()

	// Thoroughly broken NATS settings, with no NATS_URL.
	cfg, err := config.Parse(map[string]string{
		"NATS_STREAM":             "",
		"NATS_SUBJECT_PREFIX":     "*",
		"NATS_MAX_DELIVER":        "-3",
		"NATS_DUPLICATE_WINDOW":   "1ns",
		"NATS_DLQ_SUBJECT_PREFIX": "events",
	})
	if err != nil {
		t.Fatalf("broker settings were validated even though NATS_URL is empty: %v", err)
	}
	if cfg.NATS.URL != "" {
		t.Errorf("NATS_URL = %q, want empty", cfg.NATS.URL)
	}
}

// TestTheDeadLetterPrefixMayNotSitInsideTheConsumerFilter catches a configuration that looks
// fine on both lines and destroys the stream.
//
// The consumer filters {NATS_SUBJECT_PREFIX}.> and dead-letters to
// {NATS_DLQ_SUBJECT_PREFIX}.{subject}. Nest the second inside the first and every dead letter
// is redelivered to the consumer that just gave up on it, dead-lettered again, and so on --
// consumer_test.go measures 1005 handler runs for one message in two seconds.
func TestTheDeadLetterPrefixMayNotSitInsideTheConsumerFilter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, prefix, dlq string
		wantErr           bool
	}{
		{name: "the shipped defaults", prefix: "events", dlq: "dlq"},
		{name: "unrelated subtrees", prefix: "gomicro.events", dlq: "gomicro.dead"},

		// The token-aware check earns its keep here: a plain string prefix test would
		// reject this pair, which is perfectly safe.
		{name: "shared word, different token", prefix: "events", dlq: "eventsdlq"},

		{name: "nested", prefix: "events", dlq: "events.dlq", wantErr: true},
		{name: "identical", prefix: "events", dlq: "events", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Parse(map[string]string{
				"NATS_URL":                "nats://localhost:4222",
				"NATS_SUBJECT_PREFIX":     tc.prefix,
				"NATS_DLQ_SUBJECT_PREFIX": tc.dlq,
			})

			switch {
			case tc.wantErr && err == nil:
				t.Errorf("prefix %q with dead-letter prefix %q was accepted; the consumer "+
					"would be fed its own dead letters forever", tc.prefix, tc.dlq)
			case tc.wantErr && !strings.Contains(err.Error(), "dead letters"):
				t.Errorf("the error does not explain the loop: %v", err)
			case !tc.wantErr && err != nil:
				t.Errorf("prefix %q with dead-letter prefix %q was rejected: %v", tc.prefix, tc.dlq, err)
			}
		})
	}
}

// TestTheDuplicateWindowMustOutliveTheRelayRetry is the other cross-field constraint.
//
// A batch whose marking transaction fails is republished one poll interval later. If
// JetStream has forgotten the message id by then, the republish is stored as a second message
// and every consumer sees the event twice. Neither value looks wrong on its own line.
func TestTheDuplicateWindowMustOutliveTheRelayRetry(t *testing.T) {
	t.Parallel()

	_, err := config.Parse(map[string]string{
		"NATS_URL":              "nats://localhost:4222",
		"NATS_DUPLICATE_WINDOW": "5s",
		"OUTBOX_POLL_INTERVAL":  "30s",
	})
	if err == nil {
		t.Fatal("a deduplication window shorter than the relay's retry gap was accepted.\n\n" +
			"Every republished batch would be stored twice, and the duplicate would reach " +
			"every consumer.")
	}
	if !strings.Contains(err.Error(), "NATS_DUPLICATE_WINDOW") {
		t.Errorf("the error does not name the setting to change: %v", err)
	}

	// The shipped defaults are nowhere near the line.
	if _, err := config.Parse(map[string]string{"NATS_URL": "nats://localhost:4222"}); err != nil {
		t.Errorf("the default window and poll interval do not validate together: %v", err)
	}
}

// TestWildcardsAreRefusedInBrokerNames stops a subject prefix that matches everything.
func TestWildcardsAreRefusedInBrokerNames(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"NATS_STREAM", "NATS_SUBJECT_PREFIX", "NATS_CONSUMER"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()

			_, err := config.Parse(map[string]string{
				"NATS_URL": "nats://localhost:4222",
				key:        "ev>nts",
			})
			if err == nil {
				t.Errorf("%s accepted a wildcard character", key)
			}
		})
	}
}

// TestTheWorkerIsNotBoundByTheServersAuthRules is a bug found by writing a manifest.
//
// AUTH_MODE defaults to dev, and dev is refused when APP_ENV=production -- correctly, for a
// process that would otherwise serve every request as a full-scope principal. cmd/worker
// opens no listener and verifies no tokens, so the rule protects nothing there and stopped it
// booting in production entirely.
//
// Both halves are asserted, because the fix is only right if the SERVER still refuses.
func TestTheWorkerIsNotBoundByTheServersAuthRules(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		"APP_ENV":      config.EnvProduction,
		"POSTGRES_DSN": "postgres://user:pass@db:5432/gomicro",
	}

	if _, err := config.Parse(env); err == nil {
		t.Error("a traffic-serving process was allowed to start with AUTH_MODE=dev in " +
			"production; that is a total authentication bypass and must stay refused")
	}

	cfg, err := config.ParseFor(env, config.RoleWorker)
	if err != nil {
		t.Fatalf("the worker cannot start in production: %v\n\n"+
			"It has no listener and verifies no tokens, so the auth rule protects nothing "+
			"here -- it just makes the worker undeployable.", err)
	}
	if cfg.Role != config.RoleWorker {
		t.Errorf("Role = %v, want RoleWorker", cfg.Role)
	}
}

// TestTheWorkerRequiresADatabase is the requirement that IS its own.
//
// The outbox lives in Postgres, so a worker without a DSN has nothing to do. Validating it
// here rather than in main means it arrives alongside every other misconfiguration instead of
// as a separate failure after the first one is fixed.
func TestTheWorkerRequiresADatabase(t *testing.T) {
	t.Parallel()

	_, err := config.ParseFor(map[string]string{}, config.RoleWorker)
	if err == nil {
		t.Fatal("the worker validated with no POSTGRES_DSN")
	}
	if !strings.Contains(err.Error(), "POSTGRES_DSN") {
		t.Errorf("the error does not name the missing setting: %v", err)
	}

	// The SERVER has no such requirement: STORE_DRIVER=memory is the default and is what
	// makes `git clone && go run ./cmd/orderd` work with nothing installed.
	if _, err := config.Parse(map[string]string{}); err != nil {
		t.Errorf("the default server config now requires a database: %v", err)
	}
}
