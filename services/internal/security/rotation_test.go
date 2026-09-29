// Unit tests for the Phase-13.5 secrets subsystem (Task 13.5.3.5/6).
// The fault-injection drills live in drills_test.go — this file covers
// the deterministic machinery: registry policy, source adapters, the
// loader's fail-closed contract, scheduler state transitions, the JWT
// keyring swap, and the dynamic-credential swapper.
package security

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"exchange/internal/auth"
	"exchange/internal/observability"
	excerrors "exchange/pkg/errors"
)

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", code)
	}
	var e *excerrors.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("expected code %s, got %v", code, err)
	}
}

// --- Registry / policy -----------------------------------------------------

func TestDefaultRegistryCoversInventory(t *testing.T) {
	reg := DefaultRegistry()
	for _, class := range []SecretClass{
		ClassJWTSigningKey, ClassAPIKeyMaterial, ClassDBCredential,
		ClassRedisPassword, ClassAeronToken, ClassTLSCertificate,
		ClassBankingAPIKey,
	} {
		p := reg.Policy(class)
		if p.MaxAge <= 0 {
			t.Fatalf("%s: no max_age", class)
		}
	}
	if got := reg.Policy(ClassJWTSigningKey); got.Overlap != 24*time.Hour {
		t.Fatalf("jwt overlap = %s, want 24h", got.Overlap)
	}
	if got := reg.Policy(ClassTLSCertificate); got.RenewBefore != 30*24*time.Hour {
		t.Fatalf("tls renew_before = %s, want 30d", got.RenewBefore)
	}
	if !reg.Policy(ClassDBCredential).Dynamic {
		t.Fatal("db_credential must be a dynamic-lease class")
	}
}

func TestRegistryMaxAgeCeiling(t *testing.T) {
	reg := DefaultRegistry()
	// Shorter is fine (stricter); longer than 90d is rejected.
	if err := reg.SetPolicy(ClassBankingAPIKey, RotationPolicy{MaxAge: 30 * 24 * time.Hour}); err != nil {
		t.Fatalf("30d policy rejected: %v", err)
	}
	err := reg.SetPolicy(ClassBankingAPIKey, RotationPolicy{MaxAge: 120 * 24 * time.Hour})
	requireCode(t, err, CodeSecretConfigInvalid)
}

// --- VaultSource against a fake HTTP Vault ---------------------------------

// fakeVault serves the KV-v2 + dynamic-creds shapes the client consumes.
func fakeVault(t *testing.T, kv map[string]map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") == "" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"errors":["missing token"]}`)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/secret/data/"):
			path := strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")
			fields, ok := kv[path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"errors":["not found"]}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"data": fields,
					"metadata": map[string]any{
						"version":      3,
						"created_time": time.Now().UTC().Format(time.RFC3339),
					},
				},
			})
		case strings.HasPrefix(r.URL.Path, "/v1/database/creds/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"lease_id":       "database/creds/ro/abc123",
				"lease_duration": 3600,
				"renewable":      true,
				"data":           map[string]string{"username": "v-user-x", "password": "v-pass-x"},
			})
		case r.URL.Path == "/v1/sys/leases/renew" && r.Method == http.MethodPut:
			_ = json.NewEncoder(w).Encode(map[string]any{"lease_duration": 3600})
		case r.URL.Path == "/v1/sys/leases/revoke" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNoContent)
			fmt.Fprint(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"errors":["unsupported path"]}`)
		}
	}))
}

func testRefs() []SecretRef {
	return []SecretRef{
		{Name: "jwt-hs256-key", Class: ClassJWTSigningKey,
			Path: "production/secrets", Key: "jwt-hs256-key-b64", Required: true},
		{Name: "postgres-dsn", Class: ClassDBCredential,
			Path: "production/postgres", Key: "dsn", Required: true},
		{Name: "redis-password", Class: ClassRedisPassword,
			Path: "production/redis", Key: "password", Required: true},
	}
}

func TestVaultSourceRead(t *testing.T) {
	srv := fakeVault(t, map[string]map[string]string{
		"production/secrets": {"jwt-hs256-key-b64": base64.StdEncoding.EncodeToString(make([]byte, 32))},
	})
	defer srv.Close()
	src, err := NewVaultSource(VaultConfig{Addr: srv.URL, Token: "t"})
	if err != nil {
		t.Fatalf("NewVaultSource: %v", err)
	}
	v, err := src.Read(context.Background(), testRefs()[0])
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if v.Version != 3 {
		t.Fatalf("version = %d, want 3", v.Version)
	}
	if _, ok := v.Get("jwt-hs256-key-b64"); !ok {
		t.Fatal("missing payload field")
	}
	// String() must not leak field values.
	if strings.Contains(v.String(), base64.StdEncoding.EncodeToString(make([]byte, 32))) {
		t.Fatal("String() leaked secret material")
	}
}

func TestVaultSourceReadMiss(t *testing.T) {
	srv := fakeVault(t, nil)
	defer srv.Close()
	src, _ := NewVaultSource(VaultConfig{Addr: srv.URL, Token: "t"})
	if _, err := src.Read(context.Background(), testRefs()[0]); err == nil {
		t.Fatal("expected error on missing secret")
	}
}

func TestVaultSourceRejectsCleartextAndBadConfig(t *testing.T) {
	if _, err := NewVaultSource(VaultConfig{Addr: "http://vault.internal:8200", Token: "t"}); err == nil {
		t.Fatal("non-loopback plaintext http must be refused")
	}
	if _, err := NewVaultSource(VaultConfig{Addr: "http://127.0.0.1:8200", Token: "t"}); err != nil {
		t.Fatalf("loopback http should be allowed (agent/dev): %v", err)
	}
	if _, err := NewVaultSource(VaultConfig{Addr: "https://vault:8200"}); err == nil {
		t.Fatal("missing token must be refused")
	}
}

func TestVaultSourceTokenFile(t *testing.T) {
	srv := fakeVault(t, map[string]map[string]string{
		"production/secrets": {"k": "v"},
	})
	defer srv.Close()
	tok := t.TempDir() + "/token"
	if err := os.WriteFile(tok, []byte("file-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	src, err := NewVaultSource(VaultConfig{Addr: srv.URL, TokenFile: tok})
	if err != nil {
		t.Fatalf("NewVaultSource(token file): %v", err)
	}
	if _, err := src.Read(context.Background(), testRefs()[0]); err != nil {
		t.Fatalf("Read via token file: %v", err)
	}
}

func TestVaultSourceDynamicCreds(t *testing.T) {
	srv := fakeVault(t, nil)
	defer srv.Close()
	src, _ := NewVaultSource(VaultConfig{Addr: srv.URL, Token: "t"})
	cred, err := src.Issue(context.Background(), "ro")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if cred.Username != "v-user-x" || cred.TTL != time.Hour || !cred.Renewable {
		t.Fatalf("bad credential: %+v", cred)
	}
	if ttl, err := src.Renew(context.Background(), cred.LeaseID, time.Hour); err != nil || ttl != time.Hour {
		t.Fatalf("Renew: ttl=%s err=%v", ttl, err)
	}
	if err := src.Revoke(context.Background(), cred.LeaseID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
}

// --- DevSource -------------------------------------------------------------

func TestDevSourceFileAdapter(t *testing.T) {
	f := t.TempDir() + "/dev-secrets.json"
	body := `{"dev/secrets": {"jwt-hs256-key-b64": "` +
		base64.StdEncoding.EncodeToString(make([]byte, 32)) + `"}}`
	if err := os.WriteFile(f, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	src, err := NewDevFileSource(f)
	if err != nil {
		t.Fatalf("NewDevFileSource: %v", err)
	}
	if !src.IsDev() || !strings.Contains(src.Name(), "DEV") {
		t.Fatal("dev adapter must be self-labeled")
	}
	v, err := src.Read(context.Background(), SecretRef{Path: "dev/secrets"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if _, ok := v.Get("jwt-hs256-key-b64"); !ok {
		t.Fatal("payload field missing")
	}
	if _, err := src.Read(context.Background(), SecretRef{Path: "nope"}); err == nil {
		t.Fatal("missing path must error — no fabricated defaults")
	}
}

func TestDevSourceMissingFile(t *testing.T) {
	if _, err := NewDevFileSource("/nonexistent/dev-secrets.json"); err == nil {
		t.Fatal("missing file must fail construction")
	}
}

// --- Loader contract --------------------------------------------------------

func TestLoadSecretsHappyPath(t *testing.T) {
	srv := fakeVault(t, map[string]map[string]string{
		"production/secrets":  {"jwt-hs256-key-b64": "x"},
		"production/postgres": {"dsn": "postgres://u@h/db"},
		"production/redis":    {"password": "p"},
	})
	defer srv.Close()
	src, _ := NewVaultSource(VaultConfig{Addr: srv.URL, Token: "t"})
	b, err := LoadSecrets(context.Background(), src, testRefs(), true)
	if err != nil {
		t.Fatalf("LoadSecrets: %v", err)
	}
	if len(b.Secrets) != 3 || len(b.Missing) != 0 || b.Dev {
		t.Fatalf("bad bundle: %+v", b)
	}
}

func TestLoadSecretsRequiredMissFailsClosed(t *testing.T) {
	srv := fakeVault(t, map[string]map[string]string{
		"production/secrets": {"jwt-hs256-key-b64": "x"},
		// postgres + redis paths absent
	})
	defer srv.Close()
	src, _ := NewVaultSource(VaultConfig{Addr: srv.URL, Token: "t"})
	refs := append(testRefs(), SecretRef{
		Name: "optional-thing", Path: "production/missing-opt", Required: false})
	b, err := LoadSecrets(context.Background(), src, refs, true)
	if err == nil || b != nil {
		t.Fatalf("required miss must fail closed, got bundle %+v err %v", b, err)
	}
	requireCode(t, err, CodeConfigLoadFailed)
}

func TestLoadSecretsOptionalMissToleratedInDev(t *testing.T) {
	f := t.TempDir() + "/dev.json"
	if err := os.WriteFile(f, []byte(`{"dev/a": {"k": "v"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	src, err := NewDevFileSource(f)
	if err != nil {
		t.Fatal(err)
	}
	refs := []SecretRef{
		{Name: "a", Path: "dev/a", Required: true},
		{Name: "b", Path: "dev/b", Required: false},
	}
	b, err := LoadSecrets(context.Background(), src, refs, false)
	if err != nil {
		t.Fatalf("dev optional miss: %v", err)
	}
	if len(b.Missing) != 1 || b.Missing[0] != "b" || !b.Dev {
		t.Fatalf("bad bundle: %+v", b)
	}
}

func TestLoadSecretsDevAdapterRefusedWhenRequired(t *testing.T) {
	src := NewDevEnvSource(func(string) string { return "x" })
	b, err := LoadSecrets(context.Background(), src, nil, true)
	if b != nil || err == nil {
		t.Fatal("dev adapter must be refused under required=true")
	}
	requireCode(t, err, CodeConfigLoadFailed)
}

func TestSecretsRequiredSemantics(t *testing.T) {
	t.Setenv("EXC_SECRETS_REQUIRED", "production")
	if !secretsRequired("development") {
		t.Fatal("explicit production must override dev env label")
	}
	t.Setenv("EXC_SECRETS_REQUIRED", "development")
	if secretsRequired("production") {
		t.Fatal("explicit development override must downgrade")
	}
	t.Setenv("EXC_SECRETS_REQUIRED", "")
	if !secretsRequired("production") || !secretsRequired("unknown-env") || !secretsRequired("") {
		t.Fatal("production/unknown/empty env labels must fail closed to required")
	}
	if secretsRequired("staging") || secretsRequired("ci") {
		t.Fatal("non-prod labels must not require production secrets")
	}
}

// --- Scheduler --------------------------------------------------------------

func TestSchedulerStates(t *testing.T) {
	sch := NewScheduler(nil, nil, 0)
	base := time.Now()
	sch.now = func() time.Time { return base }

	sch.Track(SecretRef{Name: "fresh", Class: ClassJWTSigningKey}, 1, base.Add(-24*time.Hour))
	sch.Track(SecretRef{Name: "due", Class: ClassDBCredential}, 1,
		base.Add(-(defaultRotationAge - 7*24*time.Hour))) // 7d left → due_soon
	sch.Track(SecretRef{Name: "old", Class: ClassRedisPassword}, 1,
		base.Add(-(defaultRotationAge + time.Hour))) // past deadline → overdue
	sch.Track(SecretRef{Name: "never", Class: ClassBankingAPIKey}, 0, time.Time{})

	states := map[string]RotationState{}
	for _, st := range sch.Assess() {
		states[st.Name] = st.State
	}
	want := map[string]RotationState{
		"fresh": StateOK, "due": StateDueSoon, "old": StateOverdue, "never": StateUnrotated,
	}
	for name, ws := range want {
		if states[name] != ws {
			t.Fatalf("%s: state %s, want %s", name, states[name], ws)
		}
	}
}

func TestSchedulerRotateDueHook(t *testing.T) {
	sch := NewScheduler(nil, nil, 0)
	base := time.Now()
	sch.now = func() time.Time { return base }

	sch.Track(SecretRef{Name: "due", Class: ClassBankingAPIKey}, 1,
		base.Add(-(defaultRotationAge - time.Hour))) // inside RenewBefore window
	var calls int32
	sch.SetHook(ClassBankingAPIKey, func(_ context.Context, _ SecretSource, rec *SecretRecord) error {
		atomic.AddInt32(&calls, 1)
		rec.Version++
		return nil
	})
	out := sch.RotateDue(context.Background())
	if len(out) != 1 || out[0].State != StateOK {
		t.Fatalf("RotateDue outcome: %+v", out)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatal("hook not called")
	}
	// Manual class (no hook) is surfaced but not rotated.
	sch.Track(SecretRef{Name: "manual", Class: ClassTLSCertificate}, 1,
		base.Add(-360*24*time.Hour))
	out = sch.RotateDue(context.Background())
	for _, st := range out {
		if st.Name == "manual" {
			t.Fatalf("hook-less class must not rotate: %+v", st)
		}
	}
}

func TestSchedulerHookFailureKeepsOldMaterial(t *testing.T) {
	sch := NewScheduler(nil, nil, 0)
	base := time.Now()
	sch.now = func() time.Time { return base }
	sch.Track(SecretRef{Name: "x", Class: ClassRedisPassword}, 1,
		base.Add(-(defaultRotationAge + time.Hour)))
	sch.SetHook(ClassRedisPassword, func(context.Context, SecretSource, *SecretRecord) error {
		return errors.New("vault 503")
	})
	out := sch.RotateDue(context.Background())
	if len(out) != 1 || out[0].State != StateOverdue || out[0].LastError == "" {
		t.Fatalf("failed rotation must stay overdue with error: %+v", out)
	}
}

// --- Metrics export ---------------------------------------------------------

func TestSchedulerMetricsRender(t *testing.T) {
	reg := observability.New()
	sch := NewScheduler(nil, nil, 0)
	sch.Track(SecretRef{Name: "jwt", Class: ClassJWTSigningKey}, 1, time.Now())
	sch.RegisterMetrics(reg)
	body := reg.String()
	if !strings.Contains(body, "secret_rotation_seconds_until_expiry") ||
		!strings.Contains(body, `secret="jwt"`) {
		t.Fatalf("exposition missing secret gauge:\n%s", body)
	}
}

// --- JWTKeyring -------------------------------------------------------------

func hmacKey(n byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = n
	}
	return k
}

func TestJWTKeyringRotateAndOverlap(t *testing.T) {
	kr := NewJWTKeyring("exc.local", "exc-api", 15*time.Minute, 24*time.Hour)
	if kr.Issuer() != nil {
		t.Fatal("empty keyring must expose nil issuer (fail-closed)")
	}
	if err := kr.Rotate(JWTKeyVersion{KID: "k1", Alg: "HS256", HMACSecret: hmacKey(1)}); err != nil {
		t.Fatalf("rotate k1: %v", err)
	}
	tok, _, err := kr.Issuer().Issue("u1", auth.IssueOptions{AccountID: 7})
	if err != nil {
		t.Fatalf("issue under k1: %v", err)
	}
	// Rotate mid-flight: the old token keeps verifying under k1's kid
	// inside the 24h overlap; new issues sign under k2.
	if err := kr.Rotate(JWTKeyVersion{KID: "k2", Alg: "HS256", HMACSecret: hmacKey(2)}); err != nil {
		t.Fatalf("rotate k2: %v", err)
	}
	claims, err := kr.Issuer().Parse(tok)
	if err != nil {
		t.Fatalf("k1 token rejected inside overlap: %v", err)
	}
	if claims.KeyID != "k1" || claims.AccountID != 7 {
		t.Fatalf("bad claims: %+v", claims)
	}
	tok2, c2, err := kr.Issuer().Issue("u1", auth.IssueOptions{})
	if err != nil {
		t.Fatalf("issue under k2: %v", err)
	}
	if c2.KeyID != "k2" {
		t.Fatalf("new issue kid = %q, want k2", c2.KeyID)
	}
	if _, err := kr.Issuer().Parse(tok2); err != nil {
		t.Fatalf("k2 token rejected: %v", err)
	}
	if kr.ActiveKID() != "k2" {
		t.Fatalf("active kid = %q", kr.ActiveKID())
	}
}

func TestJWTKeyringPruneRetiresKid(t *testing.T) {
	kr := NewJWTKeyring("", "", 0, time.Millisecond)
	now := time.Now()
	kr.now = func() time.Time { return now }
	if err := kr.Rotate(JWTKeyVersion{KID: "old", Alg: "HS256", HMACSecret: hmacKey(1)}); err != nil {
		t.Fatal(err)
	}
	tok, _, _ := kr.Issuer().Issue("u", auth.IssueOptions{})
	if err := kr.Rotate(JWTKeyVersion{KID: "new", Alg: "HS256", HMACSecret: hmacKey(2)}); err != nil {
		t.Fatal(err)
	}
	// Move the clock past the overlap window, prune, verify rejection.
	now = now.Add(2 * time.Millisecond)
	kr.Prune()
	if _, err := kr.Issuer().Parse(tok); err == nil {
		t.Fatal("retired kid must reject after overlap expiry")
	}
	if len(kr.Versions()) != 1 {
		t.Fatalf("versions after prune: %v", kr.Versions())
	}
}

func TestJWTMaterialFromFields(t *testing.T) {
	good := map[string]string{
		"kid": "v1", "alg": "HS256",
		"hs256_b64": base64.StdEncoding.EncodeToString(make([]byte, 32)),
	}
	v, err := JWTMaterialFromFields(good)
	if err != nil || v.KID != "v1" || len(v.HMACSecret) != 32 {
		t.Fatalf("good HS256 fields: %+v err=%v", v, err)
	}
	if _, err := JWTMaterialFromFields(map[string]string{"kid": "x"}); err == nil {
		t.Fatal("missing material must fail")
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ed := map[string]string{
		"kid": "e1", "alg": "EdDSA",
		"ed25519_private_b64": base64.StdEncoding.EncodeToString(priv),
		"ed25519_public_b64":  base64.StdEncoding.EncodeToString(pub),
	}
	if v, err = JWTMaterialFromFields(ed); err != nil || v.EdPriv == nil {
		t.Fatalf("EdDSA fields: %+v err=%v", v, err)
	}
	if _, err := JWTMaterialFromFields(map[string]string{
		"kid": "r1", "alg": "RS256"}); err == nil {
		t.Fatal("RS256 without key material must fail")
	}
}

// --- Swapper ----------------------------------------------------------------

type fakeLeaseSource struct{ issueCalls int32 }

func (f *fakeLeaseSource) Issue(_ context.Context, role string) (*DynamicCredential, error) {
	n := atomic.AddInt32(&f.issueCalls, 1)
	return &DynamicCredential{Username: fmt.Sprintf("u%d", n),
		Password: "p", LeaseID: fmt.Sprintf("lease/%d", n),
		TTL: 20 * time.Millisecond, Renewable: true}, nil
}
func (f *fakeLeaseSource) Renew(_ context.Context, _ string, inc time.Duration) (time.Duration, error) {
	return inc, nil
}
func (f *fakeLeaseSource) Revoke(_ context.Context, _ string) error { return nil }

func TestSwapperRotateKeepsOldOnBuildFailure(t *testing.T) {
	var built int32
	sw := &Swapper[int]{
		Build: func(_ context.Context, c *DynamicCredential) (*int, error) {
			atomic.AddInt32(&built, 1)
			if c.Username == "bad" {
				return nil, errors.New("connect refused")
			}
			v := len(c.Username)
			return &v, nil
		},
	}
	ctx := context.Background()
	if err := sw.Rotate(ctx, &DynamicCredential{Username: "good"}); err != nil {
		t.Fatalf("first rotate: %v", err)
	}
	if sw.Get() == nil || *sw.Get() != 4 {
		t.Fatalf("handle after rotate: %v", sw.Get())
	}
	err := sw.Rotate(ctx, &DynamicCredential{Username: "bad"})
	requireCode(t, err, CodeSecretRotationFailed)
	if *sw.Get() != 4 {
		t.Fatal("failed build swapped in a broken handle")
	}
}

func TestLeaseRenewalIssuesAndSwaps(t *testing.T) {
	src := &fakeLeaseSource{}
	var built, closed int32
	sw := &Swapper[int]{
		Build: func(_ context.Context, c *DynamicCredential) (*int, error) {
			atomic.AddInt32(&built, 1)
			v := len(c.Username)
			return &v, nil
		},
		Close: func(*int) { atomic.AddInt32(&closed, 1) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = sw.LeaseRenewal(ctx, src, "ro", nil)
	if atomic.LoadInt32(&built) < 1 || sw.Get() == nil {
		t.Fatal("no credential swapped in")
	}
}
