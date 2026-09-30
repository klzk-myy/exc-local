// limits_service_pg_test.go — PostgreSQL-gated coverage for the
// withdrawal-cap seams added in Phase-11 Task 11.3.2 (spec §24 #79 /
// T11-006):
//
//   - PgStore.WithdrawnSince — rolling per-account hourly sum over
//     funding_transactions (statuses counted/excluded, window cutoff);
//   - PgStore.VenueWithdrawnSince — exchange-wide UTC-day sum across
//     accounts;
//   - LimitsService.CheckWithdrawal — withdraw_rate_per_hour and
//     exchange_daily_withdraw_limit (migration 272, global row only)
//     enforced through the real store.
//
// Gated on EXC_PG_TEST=1 + EXC_TEST_DSN (falls back to the
// docker-compose dev DSN) — same convention as
// option_delta_source_pg_test.go.
package risk

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// limitsPgFixture builds a scratch schema carrying the verbatim
// migrations the limits store reads: users/accounts (002/003 — account
// + kyc_tier_enum parents), funding_transactions (007), risk_limits
// (011), the RTS-9 OTR columns LoadLimits selects (047), exposure
// columns + risk_daily_usage (109), and the venue withdrawal cap (272).
func limitsPgFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run postgres-gated tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("EXC_PG_DSN")
	}
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	boot, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	schema := fmt.Sprintf("lim_it_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
	if _, err := boot.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		boot.Close(ctx)
		t.Fatalf("schema: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := pgx.Connect(c, dsn)
		if err == nil {
			conn.Exec(c, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			conn.Close(c)
		}
	})
	boot.Close(ctx)

	migDir := filepath.Join("..", "db", "migrations")
	for _, m := range []string{
		"002_create_users.up.sql",
		"003_create_accounts.up.sql",
		"007_create_funding_transactions.up.sql",
		"011_create_risk_limits.up.sql",
		"047_otr_limits.up.sql",
		"109_risk_limits_exposure.up.sql",
		"272_risk_limits_exchange_daily_cap.up.sql",
	} {
		optDeltaMig(t, ctx, dsn, schema, filepath.Join(migDir, m))
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedAccount inserts users+accounts row pairs (003 shape: user_id FK,
// account_type NOT NULL).
func seedAccount(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email) VALUES ($1, $2)`,
		id, fmt.Sprintf("u%d@lim-it", id)); err != nil {
		t.Fatalf("seed user %d: %v", id, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO accounts (id, user_id, account_type, kyc_tier)
		 VALUES ($1, $1, 'SPOT', 'T1')`, id); err != nil {
		t.Fatalf("seed account %d: %v", id, err)
	}
}

// seedWithdrawal books one funding_transactions WITHDRAWAL row.
func seedWithdrawal(t *testing.T, pool *pgxpool.Pool, accountID int64,
	amount, status string, createdAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO funding_transactions
		    (account_id, currency, type, amount, status, created_at)
		VALUES ($1, 'USD', 'WITHDRAWAL', $2::numeric, $3, $4)`,
		accountID, amount, status, createdAt); err != nil {
		t.Fatalf("seed withdrawal %s/%s: %v", amount, status, err)
	}
}

func TestPgWithdrawalCaps(t *testing.T) {
	pool := limitsPgFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedAccount(t, pool, 1)
	seedAccount(t, pool, 2)

	// Account 1 gets a 1000/h rate cap; the venue ceiling is 5000/day
	// on the global row. A scoped row smuggling a venue value must be
	// ignored by Resolve (global-only column).
	if _, err := pool.Exec(ctx, `
		INSERT INTO risk_limits (account_id, withdraw_rate_per_hour)
		VALUES (1, 1000)`); err != nil {
		t.Fatalf("insert hourly limit row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO risk_limits (exchange_daily_withdraw_limit)
		VALUES (5000)`); err != nil {
		t.Fatalf("insert venue cap row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO risk_limits (account_id, exchange_daily_withdraw_limit)
		VALUES (1, 1)`); err != nil {
		t.Fatalf("insert scoped venue row: %v", err)
	}

	// Window accounting for account 1: 400 + 400 in-window count;
	// the FAILED row and the 90-minute-old row must not.
	seedWithdrawal(t, pool, 1, "400", "PENDING", now.Add(-10*time.Minute))
	seedWithdrawal(t, pool, 1, "400", "CONFIRMED", now.Add(-30*time.Minute))
	seedWithdrawal(t, pool, 1, "300", "FAILED", now.Add(-5*time.Minute))
	seedWithdrawal(t, pool, 1, "700", "COMPLETED", now.Add(-90*time.Minute))
	// Account 2 contributes 3000 to the venue day sum only.
	seedWithdrawal(t, pool, 2, "3000", "PENDING_REVIEW", now.Add(-2*time.Minute))
	// Yesterday's rows never enter the venue-day sum.
	seedWithdrawal(t, pool, 2, "99999", "COMPLETED", now.Add(-26*time.Hour))

	// Expected venue-day total — the FAILED row and anything created
	// before today's 00:00 UTC are excluded; the -90min COMPLETED row
	// counts iff it is still the same UTC calendar day (test-time
	// dependent — computed, never assumed).
	dayStart := time.Date(now.Year(), now.Month(), now.Day(),
		0, 0, 0, 0, time.UTC)
	venueSum := int64(400 + 400 + 3000)
	if !now.Add(-90 * time.Minute).Before(dayStart) {
		venueSum += 700
	}
	headroom := int64(5000) - venueSum

	store := NewPgStore(pool)
	svc := NewLimitsService(store, nil, func() time.Time { return now })
	if err := svc.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	lim := svc.EffectiveLimits(1, "T1", "*")
	if got := lim.ExchangeDailyWithdrawLimit.String(); got != "5000" {
		t.Fatalf("venue cap %s, want 5000 (global row; scoped value ignored)", got)
	}

	// Hourly: 800 in window + 300 > 1000 → ORDER_REJECTED.
	requireCode(t, svc.CheckWithdrawal(ctx, 1, "T1", d("300")), CodeOrderRejected)
	// 800 + 200 == 1000 → boundary passes (venue 3800+200 < 5000 too).
	if err := svc.CheckWithdrawal(ctx, 1, "T1", d("200")); err != nil {
		t.Fatalf("hourly boundary must pass: %v", err)
	}

	// Venue cap: account 2 has no hourly cap, so it isolates the venue
	// check — sum + headroom == 5000 passes; +1 over rejects.
	if err := svc.CheckWithdrawal(ctx, 2, "T1", d("1")); err != nil {
		t.Fatalf("acct2 without hourly cap must pass: %v", err)
	}
	if err := svc.CheckWithdrawal(ctx, 2, "T1",
		decimal.NewFromInt(headroom)); err != nil {
		t.Fatalf("venue boundary %d+%d=5000 must pass: %v",
			venueSum, headroom, err)
	}
	requireCode(t, svc.CheckWithdrawal(ctx, 2, "T1",
		decimal.NewFromInt(headroom+1)), CodeOrderRejected)
	// Account 1 inside both caps still passes (hourly headroom = 200).
	if err := svc.CheckWithdrawal(ctx, 1, "T1", d("200")); err != nil {
		t.Fatalf("acct1 inside both caps must pass: %v", err)
	}
}

// TestPgWithdrawalCapsMigrationRoundTrip applies 272 up then down — the
// column must appear and drop cleanly in the scratch schema.
func TestPgWithdrawalCapsMigrationRoundTrip(t *testing.T) {
	pool := limitsPgFixture(t)
	ctx := context.Background()
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name='risk_limits'
		  AND column_name='exchange_daily_withdraw_limit'`).Scan(&n); err != nil {
		t.Fatalf("column probe: %v", err)
	}
	if n != 1 {
		t.Fatalf("272 must add exchange_daily_withdraw_limit, found %d", n)
	}
	migDir := filepath.Join("..", "db", "migrations")
	// Down via the pool's search_path-bound connection.
	sql, err := os.ReadFile(filepath.Join(migDir,
		"272_risk_limits_exchange_daily_cap.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name='risk_limits'
		  AND column_name='exchange_daily_withdraw_limit'`).Scan(&n); err != nil {
		t.Fatalf("post-down probe: %v", err)
	}
	if n != 0 {
		t.Fatal("exchange_daily_withdraw_limit survived the down migration")
	}
}
