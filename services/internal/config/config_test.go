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

// Phase-3 Task 3 — the new FIX surface flags validate fail-closed.
func TestFixSurfaceValidation(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	t.Setenv("EXC_SECRETS_DATA_KEY", key)
	// SP2 without TLS must refuse.
	t.Setenv("EXC_FIX_ENABLED", "true")
	t.Setenv("EXC_FIX_SP2_ENABLED", "true")
	if _, err := Load(); err == nil {
		t.Fatal("sp2_enabled without tls_enabled must fail validation")
	}
	// mTLS without a CA bundle must refuse.
	t.Setenv("EXC_FIX_SP2_ENABLED", "false")
	t.Setenv("EXC_FIX_TLS_ENABLED", "true")
	t.Setenv("EXC_FIX_TLS_CERT", "/c/t.crt")
	t.Setenv("EXC_FIX_TLS_KEY", "/c/t.key")
	t.Setenv("EXC_FIX_MTLS_REQUIRED", "true")
	if _, err := Load(); err == nil {
		t.Fatal("mtls_required without tls_ca must fail validation")
	}
	// Same-port SP2/4.4 listeners must refuse.
	t.Setenv("EXC_FIX_MTLS_REQUIRED", "false")
	t.Setenv("EXC_FIX_SP2_ENABLED", "true")
	t.Setenv("EXC_FIX_SP2_ACCEPTOR_PORT", "9879")
	if _, err := Load(); err == nil {
		t.Fatal("sp2 on the 4.4 port must fail validation")
	}
	// A complete SP2+mTLS stanza validates.
	t.Setenv("EXC_FIX_SP2_ACCEPTOR_PORT", "9880")
	t.Setenv("EXC_FIX_TLS_CA", "/c/ca.pem")
	t.Setenv("EXC_FIX_MTLS_REQUIRED", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("valid sp2+mtls stanza rejected: %v", err)
	}
	if !cfg.Fix.SP2Enabled || !cfg.Fix.MTLSRequired || cfg.Fix.SP2AcceptorPort != 9880 {
		t.Fatalf("env bindings dropped: %+v", cfg.Fix)
	}
}
