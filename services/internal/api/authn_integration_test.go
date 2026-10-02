// EXC_PG_TEST=1 gated handler-level integration for the Phase-12
// authn/2FA/profile surface — real services over dev PG + live Redis,
// real JWTs parsed back into claims for the authed calls.
//
// Run: EXC_PG_TEST=1 go test ./internal/api -run AuthnIntegration -v
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/auth"
	excredis "exchange/internal/redis"
)

type authnRig struct {
	pool   *pgxpool.Pool
	users  *auth.UserStore
	authn  *auth.AuthnService
	tfa    *auth.TwoFactorService
	keys   *auth.KeyStore
	issuer *auth.Issuer
	mail   *stubSender
}

type stubSender struct{ bodies []string }

func (s *stubSender) Send(_ context.Context, m auth.Message) error {
	s.bodies = append(s.bodies, m.Body)
	return nil
}

var linkToken = regexp.MustCompile(`token=([A-Za-z0-9_\-]+)`)

func (s *stubSender) token(t *testing.T) string {
	t.Helper()
	if len(s.bodies) == 0 {
		t.Fatal("no mail sent")
	}
	m := linkToken.FindStringSubmatch(s.bodies[len(s.bodies)-1])
	if len(m) != 2 {
		t.Fatalf("no token in mail: %q", s.bodies[len(s.bodies)-1])
	}
	return m[1]
}

func newAuthnRig(t *testing.T) *authnRig {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	rdb := excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 13)
	if err := rdb.Ping(context.Background()); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	box, err := auth.NewSecretBox([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	users, err := auth.NewUserStore(pool, box)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.NewKeyStore(pool, box)
	if err != nil {
		t.Fatal(err)
	}
	issuer := auth.NewIssuer("exc-test", "exc-api", 15*time.Minute)
	if err := issuer.AddHMACKey("k1", []byte("0123456789abcdef0123456789abcdef"), true); err != nil {
		t.Fatal(err)
	}
	sess, err := auth.NewSessionManager(auth.NewRedisSessionStore(rdb), issuer, auth.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	cache := auth.NewRedisTokenCache(rdb.Client)
	mail := &stubSender{}
	authn, err := auth.NewAuthnService(users, sess, cache, mail)
	if err != nil {
		t.Fatal(err)
	}
	tfa, err := auth.NewTwoFactorService(users, cache, sess, "exc-test")
	if err != nil {
		t.Fatal(err)
	}
	return &authnRig{pool: pool, users: users, authn: authn, tfa: tfa,
		keys: keys, issuer: issuer, mail: mail}
}

func post(t *testing.T, h http.Handler, body any, claims *auth.Claims) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(b))
	req.RemoteAddr = "198.51.100.9:7777"
	if claims != nil {
		req = req.WithContext(auth.WithClaims(req.Context(), *claims))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func get(t *testing.T, h http.Handler, claims *auth.Claims) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if claims != nil {
		req = req.WithContext(auth.WithClaims(req.Context(), *claims))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response not JSON: %s", rec.Body.String())
	}
	return m
}

// TestAuthnIntegration drives the full Phase-12 flow end-to-end:
// register → verify-email → login → profile → 2FA enroll/verify →
// api-key create under the elevated token → change-password.
func TestAuthnIntegration(t *testing.T) {
	rig := newAuthnRig(t)
	email := fmt.Sprintf("api-it-%d@test.invalid", time.Now().UnixNano())

	// register
	rec := post(t, AuthRegister(rig.authn), map[string]any{
		"email": email, "password": "sup3r-passphrase-1",
		"country": "nl", "accept_terms": true,
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	regBody := decodeBody(t, rec)
	uid := int64(regBody["user_id"].(float64))
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = rig.pool.Exec(ctx, `DELETE FROM api_keys WHERE user_id=$1`, uid)
		_, _ = rig.pool.Exec(ctx, `DELETE FROM accounts WHERE user_id=$1`, uid)
		_, _ = rig.pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, uid)
	})

	// verify-email
	rec = post(t, AuthVerifyEmail(rig.authn), map[string]any{
		"token": rig.mail.token(t),
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify-email: %d %s", rec.Code, rec.Body.String())
	}

	// login (no 2FA enrolled yet → tokens directly)
	rec = post(t, AuthLogin(rig.authn, nil), map[string]any{
		"email": email, "password": "sup3r-passphrase-1",
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	login := decodeBody(t, rec)
	if _, ok := login["requires_totp"]; ok {
		t.Fatal("login demanded TOTP before enrollment")
	}
	access := login["access_token"].(string)
	claims, err := rig.issuer.Parse(access)
	if err != nil {
		t.Fatalf("parse access token: %v", err)
	}
	if claims.TwoFactorVerified() {
		t.Fatal("fresh pwd-only session claims 2FA")
	}

	// profile GET/PUT
	rec = get(t, AccountProfileGet(rig.users), &claims)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["email"] != email {
		t.Fatalf("profile get: %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodPut, "/x", bytes.NewReader(
		[]byte(`{"full_name":"It Test","address":"1 Test St","phone":"+311234567"}`)))
	rec = httptest.NewRecorder()
	AccountProfileUpdate(rig.users)(
		rec, req.WithContext(auth.WithClaims(req.Context(), claims)))
	if rec.Code != http.StatusOK {
		t.Fatalf("profile put: %d %s", rec.Code, rec.Body.String())
	}
	if decodeBody(t, rec)["full_name"] != "It Test" {
		t.Fatalf("profile not updated: %s", rec.Body.String())
	}

	// api-key create WITHOUT 2FA elevation → TWO_FACTOR_REQUIRED.
	kc, kl, _ := AccountAPIKeys(rig.keys)
	rec = post(t, auth.RequireTwoFactor()(kc), map[string]any{
		"label": "it-key", "scopes": []string{"read"},
	}, &claims)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("api-key create without 2FA: %d %s", rec.Code, rec.Body.String())
	}

	// 2FA setup → verify → elevated token returned.
	rec = post(t, TwoFactorSetup(rig.tfa), map[string]any{}, &claims)
	if rec.Code != http.StatusOK {
		t.Fatalf("2fa setup: %d %s", rec.Code, rec.Body.String())
	}
	secret := decodeBody(t, rec)["secret"].(string)
	raw, err := auth.DecodeTOTPSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	code, err := auth.TOTPCode(raw, time.Now().UTC(),
		auth.TOTPPeriod, auth.TOTPDigits, auth.TOTPSHA1)
	if err != nil {
		t.Fatal(err)
	}
	rec = post(t, TwoFactorVerify(rig.tfa), map[string]any{"code": code}, &claims)
	if rec.Code != http.StatusOK {
		t.Fatalf("2fa verify: %d %s", rec.Code, rec.Body.String())
	}
	vbody := decodeBody(t, rec)
	if vbody["two_factor_enabled"] != true || len(vbody["backup_codes"].([]any)) != 10 {
		t.Fatalf("2fa verify response malformed: %s", rec.Body.String())
	}
	elevatedTok := vbody["access_token"].(string)
	elevated, err := rig.issuer.Parse(elevatedTok)
	if err != nil {
		t.Fatal(err)
	}
	if !elevated.TwoFactorVerified() {
		t.Fatal("elevated token lacks two_factor_verified")
	}

	// api-key create under the elevated session → 201 + secret once.
	rec = post(t, auth.RequireTwoFactor()(kc), map[string]any{
		"label": "it-key", "scopes": []string{"read"},
	}, &elevated)
	if rec.Code != http.StatusCreated {
		t.Fatalf("api-key create with 2FA: %d %s", rec.Code, rec.Body.String())
	}
	kbody := decodeBody(t, rec)
	if kbody["secret"] == "" {
		t.Fatal("hmac secret missing from create response")
	}

	// api-key list shows exactly that key, account-scoped.
	rec = get(t, kl, &elevated)
	if rec.Code != http.StatusOK {
		t.Fatalf("api-key list: %d", rec.Code)
	}
	keys := decodeBody(t, rec)["api_keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("api-key list count %d, want 1", len(keys))
	}

	// refresh the pair — rotated refresh mints totp-AMR tokens too.
	rec = post(t, AuthRefresh(rig.authn), map[string]any{
		"refresh_token": login["refresh_token"],
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body.String())
	}
	refClaims, err := rig.issuer.Parse(decodeBody(t, rec)["access_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !refClaims.TwoFactorVerified() {
		t.Fatal("refreshed token dropped the totp AMR")
	}

	// change-password: other sessions die, self survives.
	rec = post(t, AccountChangePassword(rig.authn), map[string]any{
		"current_password": "sup3r-passphrase-1",
		"new_password":     "br4nd-new-passphrase",
	}, &elevated)
	if rec.Code != http.StatusOK {
		t.Fatalf("change-password: %d %s", rec.Code, rec.Body.String())
	}
	rec = post(t, AuthLogin(rig.authn, nil), map[string]any{
		"email": email, "password": "br4nd-new-passphrase",
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login with new password: %d %s", rec.Code, rec.Body.String())
	}
	// 2FA is now enrolled → phase-1 must return the challenge.
	if decodeBody(t, rec)["requires_totp"] != true {
		t.Fatal("enrolled user login skipped the TOTP challenge")
	}
}
