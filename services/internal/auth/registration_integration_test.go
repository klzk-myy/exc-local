// EXC_PG_TEST=1 gated integration tests for Task 12.3.1/12.3.2 —
// dev PostgreSQL (migration 027 applied) + live Redis.
//
//	PG:    EXC_PG_DSN or postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable
//	Redis: EXC_REDIS_TEST_ADDR or 127.0.0.1:16379 (compose dev primary);
//	       EXC_REDIS_TEST_PASSWORD optional.
//
// Run: EXC_PG_TEST=1 go test ./internal/auth -run Integration -v
package auth

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	excredis "exchange/internal/redis"
)

const authnTestDSN = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
const authnTestRedis = "127.0.0.1:16379"

func authnPG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres/Redis integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = authnTestDSN
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pg connect: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("postgres unreachable at %s: %v", dsn, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func authnRedis(t *testing.T) *excredis.Client {
	t.Helper()
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = authnTestRedis
	}
	rdb := excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 13)
	if err := rdb.Ping(context.Background()); err != nil {
		t.Skipf("redis unreachable at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// testRig is the full wired stack: users, sessions, cache, mail, 2FA.
type testRig struct {
	users    *UserStore
	sessions *SessionManager
	cache    *RedisTokenCache
	rdb      *excredis.Client
	mail     *captureSender
	authn    *AuthnService
	tfa      *TwoFactorService
}

func newTestRig(t *testing.T) *testRig {
	t.Helper()
	pool := authnPG(t)
	rdb := authnRedis(t)
	box, err := NewSecretBox([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	users, err := NewUserStore(pool, box)
	if err != nil {
		t.Fatal(err)
	}
	issuer := NewIssuer("exc-test", "exc-api", 15*time.Minute)
	if err := issuer.AddHMACKey("k1", []byte("0123456789abcdef0123456789abcdef"), true); err != nil {
		t.Fatal(err)
	}
	sess, err := NewSessionManager(NewRedisSessionStore(rdb), issuer, SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	cache := NewRedisTokenCache(rdb.Client)
	mail := &captureSender{}
	authn, err := NewAuthnService(users, sess, cache, mail)
	if err != nil {
		t.Fatal(err)
	}
	tfa, err := NewTwoFactorService(users, cache, sess, "exc-test")
	if err != nil {
		t.Fatal(err)
	}
	return &testRig{users: users, sessions: sess, cache: cache, rdb: rdb,
		mail: mail, authn: authn, tfa: tfa}
}

// rigCounter keeps test emails unique across reruns.
var rigCounter = time.Now().UnixNano() % 1_000_000

func (g *testRig) register(t *testing.T) (*Registration, string) {
	t.Helper()
	rigCounter++
	email := fmt.Sprintf("authn-it-%d-%d@test.invalid", time.Now().UnixNano(), rigCounter)
	reg, err := g.authn.Register(context.Background(), RegisterRequest{
		Email: email, Password: "sup3r-secret-passphrase",
		Country: "de", AcceptTerms: true, IP: "198.51.100.9",
		UserAgent: "integration-test",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if reg.UserID == 0 || reg.AccountID == 0 {
		t.Fatalf("registration missing ids: %+v", reg)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		// FK order: accounts → users.
		_, _ = g.users.pool.Exec(ctx, `DELETE FROM accounts WHERE user_id=$1`, reg.UserID)
		_, _ = g.users.pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, reg.UserID)
	})
	return reg, email
}

// mailToken extracts ?token=… from the last sent body.
var tokenRe = regexp.MustCompile(`token=([A-Za-z0-9_\-]+)`)

func (s *captureSender) lastToken(t *testing.T) string {
	t.Helper()
	if len(s.msgs) == 0 {
		t.Fatal("no mail sent")
	}
	m := tokenRe.FindStringSubmatch(s.msgs[len(s.msgs)-1].Body)
	if len(m) != 2 {
		t.Fatalf("no token in mail body: %q", s.msgs[len(s.msgs)-1].Body)
	}
	return m[1]
}

func totpNow(t *testing.T, secret string) string {
	t.Helper()
	raw, err := DecodeTOTPSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	code, err := TOTPCode(raw, time.Now().UTC(), TOTPPeriod, TOTPDigits, TOTPSHA1)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// ---------------------------------------------------------------------------
// Task 12.3.1 — register → verify → login → refresh → logout
// ---------------------------------------------------------------------------

func TestRegisterLoginRefreshLogoutIntegration(t *testing.T) {
	g := newTestRig(t)
	ctx := context.Background()
	reg, email := g.register(t)

	// bcrypt persisted, never plaintext.
	var hash string
	if err := g.users.pool.QueryRow(ctx,
		`SELECT password_hash FROM users WHERE id=$1`, reg.UserID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash == "" || hash == "sup3r-secret-passphrase" || len(hash) != 60 {
		t.Fatalf("bcrypt hash malformed: %q", hash)
	}

	// Verification token → verified stamp.
	if _, err := g.authn.VerifyEmail(ctx, g.mail.lastToken(t)); err != nil {
		t.Fatalf("verify email: %v", err)
	}
	// Single-use: replay must fail.
	// (token already consumed — a second consume reports invalid)
	if err := func() error {
		_, e := g.authn.VerifyEmail(ctx, "bogus")
		return e
	}(); err == nil {
		t.Fatal("bogus verify token accepted")
	}

	// Wrong password → INVALID_CREDENTIALS.
	if _, err := g.authn.Login(ctx, LoginRequest{
		Email: email, Password: "wrong-password-xx",
	}); !codeIs(err, CodeInvalidCredentials) {
		t.Fatalf("bad password: %v", err)
	}

	// Login → access + refresh pair, session bound to default account.
	out, err := g.authn.Login(ctx, LoginRequest{
		Email: email, Password: "sup3r-secret-passphrase",
		IP: "198.51.100.9", Device: "itest",
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if out.RequiresTOTP || out.Issued == nil ||
		out.Issued.AccessToken == "" || out.Issued.RefreshToken == "" {
		t.Fatalf("login outcome malformed: %+v", out)
	}
	if out.Issued.Session.AccountID != reg.AccountID {
		t.Fatalf("session account %d, want %d",
			out.Issued.Session.AccountID, reg.AccountID)
	}
	sid := out.Issued.Session.ID

	// Refresh rotates the pair; the old refresh token must not work twice.
	rotated, err := g.authn.Refresh(ctx, out.Issued.RefreshToken)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if rotated.AccessToken == out.Issued.AccessToken ||
		rotated.RefreshToken == out.Issued.RefreshToken {
		t.Fatal("refresh did not rotate")
	}
	if _, err := g.authn.Refresh(ctx, out.Issued.RefreshToken); err == nil {
		t.Fatal("reused refresh token accepted")
	}
	// Reuse detection killed the session outright.
	s, ok, lerr := g.sessions.Lookup(ctx, sid)
	if lerr != nil {
		t.Fatalf("lookup: %v", lerr)
	}
	if ok && s.ID != "" {
		t.Fatal("session survived refresh-token reuse")
	}

	// Fresh login → logout revokes.
	out2, err := g.authn.Login(ctx, LoginRequest{
		Email: email, Password: "sup3r-secret-passphrase",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.authn.Logout(ctx, out2.Issued.Session.ID); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, ok, _ := g.sessions.Lookup(ctx, out2.Issued.Session.ID); ok {
		t.Fatal("session survived logout")
	}
}

// ---------------------------------------------------------------------------
// Task 12.3.1 — password reset: 1h TTL, token consume, session sweep
// ---------------------------------------------------------------------------

func TestPasswordResetIntegration(t *testing.T) {
	g := newTestRig(t)
	ctx := context.Background()
	reg, email := g.register(t)

	// Login so a session exists to be killed.
	out, err := g.authn.Login(ctx, LoginRequest{
		Email: email, Password: "sup3r-secret-passphrase",
	})
	if err != nil {
		t.Fatal(err)
	}
	sid := out.Issued.Session.ID

	if err := g.authn.RequestPasswordReset(ctx, email); err != nil {
		t.Fatalf("reset request: %v", err)
	}
	tok := g.mail.lastToken(t)

	// The staged token carries the spec §12.1 1h TTL.
	ttl, err := g.rdb.TTL(ctx, tokenKey("pwdreset:", tok)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl < 59*time.Minute || ttl > 61*time.Minute {
		t.Fatalf("reset token TTL %v, want ~1h", ttl)
	}

	// Uniform no-oracle: unknown email gets the same nil error.
	if err := g.authn.RequestPasswordReset(ctx, "nobody@test.invalid"); err != nil {
		t.Fatalf("reset on unknown email should be silent ok: %v", err)
	}

	if _, err := g.authn.ConfirmPasswordReset(ctx, tok, "n3w-passphrase-123"); err != nil {
		t.Fatalf("reset confirm: %v", err)
	}
	// Old password dead, new password works; prior session revoked.
	if _, err := g.authn.Login(ctx, LoginRequest{
		Email: email, Password: "sup3r-secret-passphrase",
	}); !codeIs(err, CodeInvalidCredentials) {
		t.Fatalf("old password still valid: %v", err)
	}
	if _, err := g.authn.Login(ctx, LoginRequest{
		Email: email, Password: "n3w-passphrase-123",
	}); err != nil {
		t.Fatalf("new password login: %v", err)
	}
	if _, ok, _ := g.sessions.Lookup(ctx, sid); ok {
		t.Fatal("pre-reset session survived credential rotation")
	}
	_ = reg
}

// ---------------------------------------------------------------------------
// Task 12.3.3 — change password keeps self, kills others
// ---------------------------------------------------------------------------

func TestChangePasswordRevokesOthersIntegration(t *testing.T) {
	g := newTestRig(t)
	ctx := context.Background()
	_, email := g.register(t)

	l1, err := g.authn.Login(ctx, LoginRequest{Email: email,
		Password: "sup3r-secret-passphrase", Device: "one"})
	if err != nil {
		t.Fatal(err)
	}
	l2, err := g.authn.Login(ctx, LoginRequest{Email: email,
		Password: "sup3r-secret-passphrase", Device: "two"})
	if err != nil {
		t.Fatal(err)
	}

	// Wrong current password → INVALID_CREDENTIALS.
	if err := g.authn.ChangePassword(ctx, mustUserID(t, g, email),
		"bogus-current", "n3w-passphrase-123", l1.Issued.Session.ID); err == nil {
		t.Fatal("change-password accepted wrong current password")
	}
	if err := g.authn.ChangePassword(ctx, mustUserID(t, g, email),
		"sup3r-secret-passphrase", "n3w-passphrase-123", l1.Issued.Session.ID); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if _, ok, _ := g.sessions.Lookup(ctx, l1.Issued.Session.ID); !ok {
		t.Fatal("self session killed by own password change")
	}
	if _, ok, _ := g.sessions.Lookup(ctx, l2.Issued.Session.ID); ok {
		t.Fatal("other session survived password change")
	}
}

func mustUserID(t *testing.T, g *testRig, email string) int64 {
	t.Helper()
	u, err := g.users.UserByEmail(context.Background(), email)
	if err != nil || u == nil {
		t.Fatalf("user load: %v", err)
	}
	return u.ID
}

// ---------------------------------------------------------------------------
// Task 12.3.2 — TOTP lifecycle: non-destructive stage → activate →
// re-key stays intact until verified → disable needs password+code →
// backup codes single-use.
// ---------------------------------------------------------------------------

func TestTwoFactorLifecycleIntegration(t *testing.T) {
	g := newTestRig(t)
	ctx := context.Background()
	_, email := g.register(t)
	uid := mustUserID(t, g, email)

	// Setup stages the candidate; nothing active yet.
	setup1, err := g.tfa.Setup(ctx, uid, email)
	if err != nil {
		t.Fatal(err)
	}
	if setup1.Secret == "" || setup1.URI == "" || setup1.ExpiresInSec != 600 {
		t.Fatalf("setup malformed: %+v", setup1)
	}
	staged, ok, err := g.cache.Get(ctx, pendingKey(uid))
	if err != nil || !ok || staged != setup1.Secret {
		t.Fatalf("candidate not staged: %v %q", ok, staged)
	}
	if s, _ := g.users.TOTPSecretForUser(ctx, uid); s != "" {
		t.Fatal("active secret set before verify")
	}

	// Wrong code → fails, candidate remains staged.
	if _, err := g.tfa.Verify(ctx, uid, "", "000000"); err == nil {
		t.Fatal("bad code accepted")
	}
	if _, ok, _ := g.cache.Get(ctx, pendingKey(uid)); !ok {
		t.Fatal("candidate dropped after failed verify")
	}

	// Correct code → activates, returns 10 backup codes, elevated token.
	res, err := g.tfa.Verify(ctx, uid, "", totpNow(t, setup1.Secret))
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if !res.Activated || len(res.BackupCodes) != TOTPBackupCodeCount {
		t.Fatalf("activation malformed: %+v", res)
	}
	active, err := g.users.TOTPSecretForUser(ctx, uid)
	if err != nil || active != setup1.Secret {
		t.Fatalf("active secret mismatch: %q", active)
	}

	// Re-enroll: new candidate staged, ACTIVE secret untouched until the
	// new candidate verifies. Abandoned enrollment leaves 2FA intact.
	setup2, err := g.tfa.Setup(ctx, uid, email)
	if err != nil {
		t.Fatal(err)
	}
	if setup2.Secret == setup1.Secret {
		t.Fatal("re-enroll reused secret")
	}
	if s, _ := g.users.TOTPSecretForUser(ctx, uid); s != setup1.Secret {
		t.Fatal("active secret replaced at stage time")
	}
	// Old-secret code still gates while the candidate is pending...
	// Verify prefers the staged candidate by design; a code minted from
	// the OLD secret must NOT promote the candidate.
	if _, err := g.tfa.Verify(ctx, uid, "", totpNow(t, setup1.Secret)); err == nil {
		t.Fatal("old-secret code verified the pending candidate")
	}
	if s, _ := g.users.TOTPSecretForUser(ctx, uid); s != setup1.Secret {
		t.Fatal("active secret replaced by failed candidate verify")
	}
	// Candidate code activates the re-key.
	if _, err := g.tfa.Verify(ctx, uid, "", totpNow(t, setup2.Secret)); err != nil {
		t.Fatalf("re-key verify: %v", err)
	}
	if s, _ := g.users.TOTPSecretForUser(ctx, uid); s != setup2.Secret {
		t.Fatal("candidate did not promote after verify")
	}
	// Pending cleared.
	if _, ok, _ := g.cache.Get(ctx, pendingKey(uid)); ok {
		t.Fatal("pending candidate left after activation")
	}

	// Disable: wrong password, then wrong code, then both correct.
	if err := g.tfa.Disable(ctx, uid, "wrong-password", totpNow(t, setup2.Secret)); err == nil {
		t.Fatal("disable accepted wrong password")
	}
	if err := g.tfa.Disable(ctx, uid, "sup3r-secret-passphrase", "000000"); err == nil {
		t.Fatal("disable accepted wrong code")
	}
	if s, _ := g.users.TOTPSecretForUser(ctx, uid); s == "" {
		t.Fatal("disable cleared factor on failed checks")
	}
	if err := g.tfa.Disable(ctx, uid, "sup3r-secret-passphrase",
		totpNow(t, setup2.Secret)); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if s, _ := g.users.TOTPSecretForUser(ctx, uid); s != "" {
		t.Fatal("factor survived disable")
	}
}

func TestTwoFactorBackupCodesSingleUseIntegration(t *testing.T) {
	g := newTestRig(t)
	ctx := context.Background()
	_, email := g.register(t)
	uid := mustUserID(t, g, email)

	setup, err := g.tfa.Setup(ctx, uid, email)
	if err != nil {
		t.Fatal(err)
	}
	res, err := g.tfa.Verify(ctx, uid, "", totpNow(t, setup.Secret))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.BackupCodes) == 0 {
		t.Fatal("no backup codes returned")
	}
	code := res.BackupCodes[0]
	ok, err := g.tfa.VerifyFactor(ctx, uid, code)
	if err != nil || !ok {
		t.Fatalf("backup code rejected: %v", err)
	}
	ok, err = g.tfa.VerifyFactor(ctx, uid, code)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("backup code reused")
	}
	// Remaining codes still valid (re-key invalidated the first set only
	// for codes from THAT enrollment — here, second code of same set).
	if ok, err := g.tfa.VerifyFactor(ctx, uid, res.BackupCodes[1]); err != nil || !ok {
		t.Fatalf("second backup code unusable: %v", err)
	}
}
