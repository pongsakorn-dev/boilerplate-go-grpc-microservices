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

// TestValidateRefusesOIDCUntilTheVerifierExists guards the bug that made this whole file
// worth re-reading.
//
// The server used to install the dev interceptor unconditionally and never read AuthMode at
// all. So APP_ENV=production AUTH_MODE=oidc validated, booted with no warning, and served
// every request as a full-scope dev-tenant principal -- and `oidc` is precisely the value a
// reader sets BECAUSE they read the warning about dev mode.
//
// Until the verifier exists, the only honest behaviour is to refuse. Delete this test in M5,
// and replace it with one asserting a real token is required.
func TestValidateRefusesOIDCUntilTheVerifierExists(t *testing.T) {
	t.Parallel()

	for _, env := range []string{config.EnvDevelopment, config.EnvStaging, config.EnvProduction} {
		_, err := config.Parse(map[string]string{
			"APP_ENV":         env,
			"AUTH_MODE":       config.AuthOIDC,
			"OIDC_ISSUER_URL": "https://issuer.example.com",
			"OIDC_AUDIENCE":   "orders",
		})
		if err == nil {
			t.Errorf("APP_ENV=%s AUTH_MODE=oidc was accepted, but no OIDC verifier exists -- "+
				"the service would run with NO authentication while appearing configured", env)
			continue
		}
		if !strings.Contains(err.Error(), "not implemented") {
			t.Errorf("APP_ENV=%s: error does not explain why oidc is refused: %v", env, err)
		}
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
