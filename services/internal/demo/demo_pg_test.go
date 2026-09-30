// EXC_PG_TEST=1 gated integration tests for Task 8.5.3.2 — dev
// PostgreSQL (migration 238 applied) + live Redis for the auth-side
// registration exercise.
//
//	PG:    EXC_PG_DSN or postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable
//	Redis: EXC_REDIS_TEST_ADDR or 127.0.0.1:16379 (compose dev primary)
//
// Run: EXC_PG_TEST=1 go test ./internal/demo -v
package demo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/auth"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

func demoPG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres integration tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pg connect: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("postgres unreachable at %s: %v", dsn, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedUser inserts a throwaway users row and returns its id.
func seedUser(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var uid int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email, status) VALUES ($1,'ACTIVE') RETURNING id`,
		fmt.Sprintf("demo_it_%d@example.com", time.Now().UnixNano())).Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM admin_audit_log WHERE target_type='account'
			AND target_id IN (SELECT id FROM accounts WHERE user_id=$1)`, uid)
		_, _ = pool.Exec(ctx, `DELETE FROM account_closures WHERE user_id=$1`, uid)
		_, _ = pool.Exec(ctx, `DELETE FROM balances WHERE account_id IN
			(SELECT id FROM accounts WHERE user_id=$1)`, uid)
		_, _ = pool.Exec(ctx, `DELETE FROM accounts WHERE user_id=$1`, uid)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, uid)
	})
	return uid
}

func TestProvisionIntegration(t *testing.T) {
	pool := demoPG(t)
	svc := New(pool, "demo")
	uid := seedUser(t, pool)
	ctx := context.Background()

	before := time.Now().UTC()
	aid, err := svc.Provision(ctx, uid)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	var typ, status string
	var expires time.Time
	if err := pool.QueryRow(ctx,
		`SELECT account_type::text, status::text, demo_expires_at
		   FROM accounts WHERE id=$1`, aid).Scan(&typ, &status, &expires); err != nil {
		t.Fatalf("account read: %v", err)
	}
	if typ != "DEMO" || status != "ACTIVE" {
		t.Fatalf("account %s/%s, want DEMO/ACTIVE", typ, status)
	}
	want := before.Add(DefaultLifetime)
	if expires.Before(want.Add(-time.Minute)) || expires.After(want.Add(2*time.Minute)) {
		t.Fatalf("demo_expires_at %s, want ~%s", expires, want)
	}
	var avail string
	if err := pool.QueryRow(ctx,
		`SELECT available::text FROM balances WHERE account_id=$1 AND currency='USD'`,
		aid).Scan(&avail); err != nil {
		t.Fatalf("balance read: %v", err)
	}
	got, _ := decimal.NewFromString(avail)
	if !got.Equal(DefaultBalanceUSD) {
		t.Fatalf("seeded USD %s, want %s", avail, DefaultBalanceUSD)
	}

	// Configurable balance override.
	svc.WithInitialBalance(decimal.NewFromInt(7_500))
	uid2 := seedUser(t, pool)
	aid2, err := svc.Provision(ctx, uid2)
	if err != nil {
		t.Fatalf("provision2: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT available::text FROM balances WHERE account_id=$1 AND currency='USD'`,
		aid2).Scan(&avail); err != nil {
		t.Fatalf("balance2 read: %v", err)
	}
	if avail != "7500.00000000" {
		t.Fatalf("custom seed %s, want 7500.00000000", avail)
	}
}

func TestIsDemoAndFundingGateIntegration(t *testing.T) {
	pool := demoPG(t)
	svc := New(pool, "demo")
	uid := seedUser(t, pool)
	ctx := context.Background()

	demoAID, err := svc.Provision(ctx, uid)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	var spotAID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type) VALUES ($1,'SPOT') RETURNING id`,
		uid).Scan(&spotAID); err != nil {
		t.Fatalf("seed spot account: %v", err)
	}

	if ok, err := svc.IsDemo(ctx, demoAID); err != nil || !ok {
		t.Fatalf("IsDemo(%d) = %v, %v — want true", demoAID, ok, err)
	}
	if ok, err := svc.IsDemo(ctx, spotAID); err != nil || ok {
		t.Fatalf("IsDemo(%d) = %v, %v — want false", spotAID, ok, err)
	}

	gate := svc.WrapFundingChecker(nil) // inner nil: demo check still fires
	var e *excerrors.Error
	if err := gate.AssertMutable(ctx, demoAID); err == nil ||
		!errors.As(err, &e) || e.Code != "FORBIDDEN" {
		t.Fatalf("demo funding op err %v, want FORBIDDEN", err)
	}
	if err := gate.AssertMutable(ctx, spotAID); err != nil {
		t.Fatalf("spot funding op rejected: %v", err)
	}
}

func TestTouchActivityIntegration(t *testing.T) {
	pool := demoPG(t)
	svc := New(pool, "demo")
	uid := seedUser(t, pool)
	ctx := context.Background()
	aid, err := svc.Provision(ctx, uid)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	// Force the deadline into the past, then a touch must re-arm it.
	if _, err := pool.Exec(ctx,
		`UPDATE accounts SET demo_expires_at = now() - interval '1 hour' WHERE id=$1`,
		aid); err != nil {
		t.Fatal(err)
	}
	if err := svc.TouchActivity(ctx, aid); err != nil {
		t.Fatalf("touch: %v", err)
	}
	var expires time.Time
	if err := pool.QueryRow(ctx,
		`SELECT demo_expires_at FROM accounts WHERE id=$1`, aid).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if expires.Before(time.Now().UTC().Add(DefaultLifetime - time.Hour)) {
		t.Fatalf("deadline not re-armed: %s", expires)
	}
	// Non-demo accounts are a no-op.
	var spotAID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type) VALUES ($1,'SPOT') RETURNING id`,
		uid).Scan(&spotAID); err != nil {
		t.Fatal(err)
	}
	if err := svc.TouchActivity(ctx, spotAID); err != nil {
		t.Fatalf("touch on spot: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE id=$1 AND demo_expires_at IS NOT NULL`,
		spotAID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("spot account gained a demo deadline (n=%d, err=%v)", n, err)
	}
}

// fakeCanceller records MassCancelAccount calls.
type fakeCanceller struct{ calls []int64 }

func (f *fakeCanceller) MassCancelAccount(_ context.Context, id int64, _ string) (int, error) {
	f.calls = append(f.calls, id)
	return 0, nil
}

type fakeTerminator struct{ calls []int64 }

func (f *fakeTerminator) RevokeAllExcept(_ context.Context, id int64, _ string, _ string) (int, error) {
	f.calls = append(f.calls, id)
	return 0, nil
}

type fakeRevoker struct{ calls []int64 }

func (f *fakeRevoker) RevokeAllKeys(_ context.Context, id int64, _ string) (int, error) {
	f.calls = append(f.calls, id)
	return 0, nil
}

func TestExpireSweepIntegration(t *testing.T) {
	pool := demoPG(t)
	svc := New(pool, "demo")
	fc := &fakeCanceller{}
	ft := &fakeTerminator{}
	fk := &fakeRevoker{}
	svc.WithOrderCanceller(fc).WithSessionTerminator(ft).WithCredentialRevoker(fk)
	uid := seedUser(t, pool)
	ctx := context.Background()

	// One expired DEMO account, one live DEMO account, one expired SPOT.
	expiredAID, err := svc.Provision(ctx, uid)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE accounts SET demo_expires_at = now() - interval '1 hour' WHERE id=$1`,
		expiredAID); err != nil {
		t.Fatal(err)
	}
	liveAID, err := svc.Provision(ctx, uid)
	if err != nil {
		t.Fatalf("provision live: %v", err)
	}
	var spotAID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type) VALUES ($1,'SPOT') RETURNING id`,
		uid).Scan(&spotAID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE accounts SET demo_expires_at = now() - interval '1 hour' WHERE id=$1`,
		spotAID); err != nil {
		t.Fatal(err)
	}

	n, err := svc.ExpireSweep(ctx, 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("closed %d, want 1", n)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM accounts WHERE id=$1`, expiredAID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "CLOSED" {
		t.Fatalf("expired account status %s, want CLOSED", status)
	}
	// Closure-convention rows landed in the same tx.
	var reason string
	var forced bool
	if err := pool.QueryRow(ctx,
		`SELECT reason, forced FROM account_closures WHERE account_id=$1`,
		expiredAID).Scan(&reason, &forced); err != nil {
		t.Fatalf("closure row: %v", err)
	}
	if reason != ExpiryReason || !forced {
		t.Fatalf("closure row %q/forced=%v", reason, forced)
	}
	var action string
	if err := pool.QueryRow(ctx,
		`SELECT action FROM admin_audit_log
		  WHERE target_type='account' AND target_id=$1
		  ORDER BY id DESC LIMIT 1`, expiredAID).Scan(&action); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if action != "account.close" {
		t.Fatalf("audit action %q, want account.close", action)
	}
	// Survivors untouched.
	for _, id := range []int64{liveAID, spotAID} {
		if err := pool.QueryRow(ctx,
			`SELECT status::text FROM accounts WHERE id=$1`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "ACTIVE" {
			t.Fatalf("account %d status %s, want ACTIVE", id, status)
		}
	}
	// Post-commit revocation seams fired.
	if len(fc.calls) != 1 || fc.calls[0] != expiredAID {
		t.Fatalf("canceller calls %v", fc.calls)
	}
	if len(ft.calls) != 1 || ft.calls[0] != expiredAID {
		t.Fatalf("terminator calls %v", ft.calls)
	}
	if len(fk.calls) != 1 || fk.calls[0] != expiredAID {
		t.Fatalf("revoker calls %v", fk.calls)
	}
	// Idempotent: a second sweep closes nothing.
	if n, err := svc.ExpireSweep(ctx, 100); err != nil || n != 0 {
		t.Fatalf("resweep closed %d (err %v), want 0", n, err)
	}
}

// ---------------------------------------------------------------------------
// Registration wiring — demo env mints DEMO+seed in the register tx
// ---------------------------------------------------------------------------

func TestRegisterProvisionsDemoIntegration(t *testing.T) {
	pool := demoPG(t)
	rdb := excredis.New(func() string {
		if a := os.Getenv("EXC_REDIS_TEST_ADDR"); a != "" {
			return a
		}
		return "127.0.0.1:16379"
	}(), os.Getenv("EXC_REDIS_TEST_PASSWORD"), 0)
	if err := rdb.Ping(context.Background()); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}
	defer func() { _ = rdb.Close() }()

	svc := New(pool, "demo")
	users, err := auth.NewUserStore(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	issuer := auth.NewIssuer("exc-test", "exc-api", 15*time.Minute)
	if err := issuer.AddHMACKey("k1",
		[]byte("0123456789abcdef0123456789abcdef"), true); err != nil {
		t.Fatal(err)
	}
	sess, err := auth.NewSessionManager(auth.NewRedisSessionStore(rdb),
		issuer, auth.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	authn, err := auth.NewAuthnService(users, sess,
		auth.NewRedisTokenCache(rdb.Client), auth.NewLogSender(nil))
	if err != nil {
		t.Fatal(err)
	}
	authn.WithDemo(svc)

	email := fmt.Sprintf("demo-reg-%d@test.invalid", time.Now().UnixNano())
	reg, err := authn.Register(context.Background(), auth.RegisterRequest{
		Email: email, Password: "sup3r-secret-passphrase",
		Country: "de", AcceptTerms: true,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM balances WHERE account_id=$1`, reg.AccountID)
		_, _ = pool.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, reg.AccountID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, reg.UserID)
	})
	var typ string
	var expires *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT account_type::text, demo_expires_at FROM accounts WHERE id=$1`,
		reg.AccountID).Scan(&typ, &expires); err != nil {
		t.Fatal(err)
	}
	if typ != "DEMO" || expires == nil {
		t.Fatalf("registered account %s (expires %v), want DEMO+deadline",
			typ, expires)
	}
	var avail string
	if err := pool.QueryRow(ctx,
		`SELECT available::text FROM balances WHERE account_id=$1 AND currency='USD'`,
		reg.AccountID).Scan(&avail); err != nil {
		t.Fatalf("seeded balance: %v", err)
	}
	got, _ := decimal.NewFromString(avail)
	if !got.Equal(DefaultBalanceUSD) {
		t.Fatalf("seeded USD %s, want %s", avail, DefaultBalanceUSD)
	}
}
