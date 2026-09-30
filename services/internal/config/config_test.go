package config

import (
	"encoding/base64"
	"testing"
)

// Regression: viper's AutomaticEnv only surfaces env vars for keys that
// exist in AllKeys — i.e. keys with a registered default or a config-file
// entry. Scalar keys with no default (secrets.data_key, redis.password,
// fix.tls_cert/key/ca) were silently dropped, so the shipped Kubernetes
// pattern — EXC_SECRETS_DATA_KEY injected via secretKeyRef with no
// config.yaml — could never satisfy production validation.
func TestLoadEnvOnlyBindings(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	t.Setenv("EXC_ENVIRONMENT", "production")
	t.Setenv("EXC_SECRETS_DATA_KEY", key)
	t.Setenv("EXC_REDIS_PASSWORD", "pw-from-env")
	t.Setenv("EXC_FIX_TLS_CERT", "/certs/tls.crt")
	// No config file on the search path (test cwd has none) — the load
	// must succeed purely on defaults + env.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Secrets.DataKey != key {
		t.Fatalf("secrets.data_key: env value not bound (got %q)", cfg.Secrets.DataKey)
	}
	if cfg.Redis.Password != "pw-from-env" {
		t.Fatalf("redis.password: env value not bound (got %q)", cfg.Redis.Password)
	}
	if cfg.Fix.TLSCert != "/certs/tls.crt" {
		t.Fatalf("fix.tls_cert: env value not bound (got %q)", cfg.Fix.TLSCert)
	}
}
