// rollover_service_test.go — Task 3.3.7 / 3.3.22 / 3.3.23 coverage.
//
// Pure tests cover the DST-aware 17:00 ET trigger (EDT vs EST — summer is
// the EARLIER UTC hour, remediation #35), the weekday-only fire schedule
// and the grace-period fee boundary. Integration tests (gated by
// EXC_PG_TEST=1, dev Postgres :5433 + Redis :16379 — same convention as
// ledger_service_test.go) run the whole roll against a scratch schema
// with the real migrations applied.
package settlement

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	excredis "exchange/internal/redis"
)

var ny = func() *time.Location {
	l, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return l
}()

func utc(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad fixture time %s", s)
	}
	return ts
}

// ---------------------------------------------------------------------------
// Trigger schedule — DST mapping (remediation #35: summer EDT = the EARLIER
// UTC hour).
// ---------------------------------------------------------------------------

func TestRolloverCutoff_EDT(t *testing.T) {
	rc, err := DefaultRolloverClock()
	if err != nil {
		t.Fatal(err)
	}
	// 2025-07-01 is a Tuesday; 12:00 UTC = 08:00 EDT — next cutoff is
	// 17:00 EDT = 21:00 UTC same day.
	fire := rc.NextCutoff(utc(t, "2025-07-01T12:00:00Z"))
	if fire.UTC().Hour() != 21 || fire.UTC().Minute() != 0 ||
		fire.UTC().Format("2006-01-02") != "2025-07-01" {
		t.Fatalf("EDT fire = %s (want 2025-07-01 21:00 UTC)", fire.UTC())
	}
	if et := fire.In(ny); et.Hour() != 17 || et.Format("MST") != "EDT" {
		t.Fatalf("fire ET = %s (want 17:00 EDT)", et)
	}
}

func TestRolloverCutoff_EST(t *testing.T) {
	rc, err := DefaultRolloverClock()
	if err != nil {
		t.Fatal(err)
	}
	// 2025-01-15 is a Wednesday; 17:00 EST = 22:00 UTC.
	fire := rc.NextCutoff(utc(t, "2025-01-15T12:00:00Z"))
	if fire.UTC().Hour() != 22 || fire.UTC().Format("2006-01-02") != "2025-01-15" {
		t.Fatalf("EST fire = %s (want 2025-01-15 22:00 UTC)", fire.UTC())
	}
	if et := fire.In(ny); et.Hour() != 17 || et.Format("MST") != "EST" {
		t.Fatalf("fire ET = %s (want 17:00 EST)", et)
	}
}

func TestRolloverCutoff_AfterCutoff(t *testing.T) {
	rc, _ := DefaultRolloverClock()
	// 21:30 UTC Tuesday = 17:30 EDT — today's fire has passed; next is
	// Wednesday 21:00 UTC.
	if fire := rc.NextCutoff(utc(t, "2025-07-01T21:30:00Z")); fire.UTC().Format("2006-01-02 15:04") != "2025-07-02 21:00" {
		t.Fatalf("post-cutoff fire = %s", fire.UTC())
	}
}

func TestNextWeekdayCutoff_WeekendSkip(t *testing.T) {
	rc, _ := DefaultRolloverClock()
	// Friday 2025-07-04 22:00 UTC = 18:00 EDT (post-fire): next weekday
	// cutoff must be Monday 2025-07-07 17:00 EDT = 21:00 UTC — the daemon
	// never fires Saturday/Sunday.
	fire := nextWeekdayCutoff(rc, utc(t, "2025-07-04T22:00:00Z"))
	if fire.UTC().Format("2006-01-02 15:04") != "2025-07-07 21:00" {
		t.Fatalf("weekend-skip fire = %s", fire.UTC())
	}
	if wd := fire.In(ny).Weekday(); wd == time.Saturday || wd == time.Sunday {
		t.Fatalf("fire landed on %s", wd)
	}
}

// ---------------------------------------------------------------------------
// Swap-free admin fee — pure grace-boundary math (Task 3.3.23).
// ---------------------------------------------------------------------------

func TestComputeAdminFee_GraceBoundary(t *testing.T) {
	lots := decimal.NewFromInt(2)
	rate := decimal.RequireFromString("25.5")
	// Day 5 of a 5-day grace: still inside — no fee.
	if fee, ok := ComputeAdminFee(lots, 5, 5, rate); ok || !fee.IsZero() {
		t.Fatalf("holding=5 grace=5 → fee=%s ok=%v, want no charge", fee, ok)
	}
	// Day 6: beyond grace — fee = lots × rate.
	fee, ok := ComputeAdminFee(lots, 6, 5, rate)
	if !ok || !fee.Equal(decimal.RequireFromString("51")) {
		t.Fatalf("holding=6 → fee=%s ok=%v, want 51", fee, ok)
	}
	// Zero-rate schedule never charges.
	if _, ok := ComputeAdminFee(lots, 30, 5, decimal.Zero); ok {
		t.Fatal("zero rate must not charge")
	}
}

// ---------------------------------------------------------------------------
// Integration — scratch schema, real migrations 001..118 subset + fixtures.
// ---------------------------------------------------------------------------

// rolloverFixture applies the migration subset the rollover path touches
// (plus the §5.3 ledger_entries/journal_sums fixture owned by migration
// 102) into a throwaway schema and returns the wired services.
func rolloverFixture(t *testing.T) (*RolloverService, *pgxpool.Pool, *fakePub) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres/Redis integration tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = defaultTestDSN
	}
	schema := itestSchema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	boot, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable (%v)", err)
	}
	if err := boot.Ping(ctx); err != nil {
		boot.Close(ctx)
		t.Skipf("postgres unreachable (%v)", err)
	}
	if _, err := boot.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		boot.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		c2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		conn, err := pgx.Connect(c2, dsn)
		if err == nil {
			_, _ = conn.Exec(c2, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			conn.Close(c2)
		}
	})
	boot.Close(ctx)

	migDir := "../db/migrations"
	for _, m := range []string{
		"001_create_instruments.up.sql",
		"002_create_users.up.sql",
		"003_create_accounts.up.sql",
		"004_create_balances.up.sql",
		"005_create_orders.up.sql",
		"014_create_positions.up.sql",
		"036_create_general_ledger.up.sql",
		"088_gl_chart_of_accounts.up.sql",
		"104_accounts_settlement_intent.up.sql",
		"105_swap_free_admin_fees.up.sql",
		"114_swap_rates.up.sql",
		"116_accounts_swapfree_status.up.sql",
		"117_swap_free_admin_fee_assessments.up.sql",
		"118_positions_rollover_columns.up.sql",
	} {
		migExec(t, ctx, dsn, schema, migDir+"/"+m)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	for _, ddl := range strings.Split(ledgerEntriesFixture, ";") {
		if ddl = strings.TrimSpace(ddl); ddl != "" {
			if _, err := pool.Exec(ctx, ddl); err != nil {
				t.Fatalf("fixture: %v", err)
			}
		}
	}

	rdb := excredis.New(defaultTestRedis, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 0)
	if addr := os.Getenv("EXC_REDIS_TEST_ADDR"); addr != "" {
		rdb = excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 0)
	}
	if err := rdb.Ping(ctx); err != nil {
		t.Skipf("redis unreachable (%v)", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	pub := &fakePub{}
	ledgerSvc, err := NewLedgerService(pool, rdb, pub)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	cal := testCalendar(t)
	store := NewPgSwapRateStore(pool)
	engine, err := NewSwapEngine(store, store, cal, ledgerSvc, pub, nil, nil)
	if err != nil {
		t.Fatalf("swap engine: %v", err)
	}
	fees, err := NewSwapFreeFeeService(pool, ledgerSvc)
	if err != nil {
		t.Fatalf("fees: %v", err)
	}
	svc, err := NewRolloverService(pool, engine, cal, fees, rdb)
	if err != nil {
		t.Fatalf("rollover: %v", err)
	}
	return svc, pool, pub
}

// rollAccount seeds a user+account with the given intent/swapfree flags.
func rollAccount(t *testing.T, pool *pgxpool.Pool, intent, swapfree string) int64 {
	t.Helper()
	ctx := context.Background()
	var uid, aid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status) VALUES ($1,'ACTIVE') RETURNING id`,
		fmt.Sprintf("ro_%d@example.com", time.Now().UnixNano())).Scan(&uid); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO accounts (user_id, account_type, settlement_intent, swapfree_status)
		VALUES ($1,'MARGIN',$2::settlement_intent_enum,$3) RETURNING id`,
		uid, intent, swapfree).Scan(&aid); err != nil {
		t.Fatalf("account: %v", err)
	}
	return aid
}

func rollPosition(t *testing.T, pool *pgxpool.Pool, accountID int64, qty, entry, opened string) int64 {
	t.Helper()
	var pid int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO positions (account_id, instrument_id, side, quantity,
		                       entry_price, opened_at)
		VALUES ($1, 1, 'LONG', $2::numeric, $3::numeric, $4) RETURNING id`,
		accountID, qty, entry, opened).Scan(&pid); err != nil {
		t.Fatalf("position: %v", err)
	}
	return pid
}

func usdBalance(t *testing.T, pool *pgxpool.Pool, accountID int64) decimal.Decimal {
	t.Helper()
	var total decimal.Decimal
	err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(total,0) FROM balances WHERE account_id=$1 AND currency='USD'`,
		accountID).Scan(&total)
	if err != nil {
		if err == pgx.ErrNoRows {
			return decimal.Zero
		}
		t.Fatalf("balance: %v", err)
	}
	return total
}

func seedRateAndMarkup(t *testing.T, pool *pgxpool.Pool, rollDate string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO swap_rates (instrument_id, effective_date, long_swap_points, short_swap_points, source)
		VALUES (1, $1, 0.000021, -0.000025, 'TEST')
		ON CONFLICT (instrument_id, effective_date) DO NOTHING`, rollDate); err != nil {
		t.Fatalf("swap rate: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO swap_markup_policies
		    (instrument_id, long_markup_bps, short_markup_bps, status, proposed_by, approved_by)
		VALUES (NULL, 0, 0, 'ACTIVE', 'prop', 'appr')`); err != nil {
		t.Fatalf("markup policy: %v", err)
	}
}

// TestIntegration_RolloverTuesday — a plain weekday roll: VD advances to
// the Friday after the T+2 spot date, 1 day of financing, balanced
// journal, value date moved.
func TestIntegration_RolloverTuesday(t *testing.T) {
	svc, pool, _ := rolloverFixture(t)
	ctx := context.Background()

	// Tuesday 2025-07-15 17:00 EDT. T+2 lag: spot date Thu 07-17, rolled to
	// Fri 07-18 → 1 day.
	rollNow := utc(t, "2025-07-15T21:00:00Z")
	seedRateAndMarkup(t, pool, "2025-07-15")

	acct := rollAccount(t, pool, "ROLLING_MARGIN", "STANDARD")
	pos := rollPosition(t, pool, acct, "100000", "1.1000", "2025-07-14T10:00:00Z")

	rep, err := svc.RunOnce(ctx, rollNow)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rep.Skipped || rep.PositionsScanned != 1 || rep.PositionsRolled != 1 || len(rep.Errors) != 0 {
		t.Fatalf("report %+v", rep)
	}

	// charge = 100000 × 0.000021 × 1 = 2.10 USD credit (positive long
	// points accrue TO the holder).
	if bal := usdBalance(t, pool, acct); !bal.Equal(decimal.RequireFromString("2.1")) {
		t.Fatalf("balance=%s want 2.1", bal)
	}
	var vd time.Time
	if err := pool.QueryRow(ctx, `SELECT value_date FROM positions WHERE id=$1`, pos).Scan(&vd); err != nil {
		t.Fatal(err)
	}
	if vd.Format("2006-01-02") != "2025-07-18" {
		t.Fatalf("value_date=%s want 2025-07-18", vd.Format("2006-01-02"))
	}
	var days int
	var jeID *int64
	if err := pool.QueryRow(ctx,
		`SELECT days, journal_entry_id FROM swap_accrual_records WHERE position_id=$1`, pos).
		Scan(&days, &jeID); err != nil {
		t.Fatalf("accrual: %v", err)
	}
	if days != 1 || jeID == nil {
		t.Fatalf("accrual days=%d journal=%v", days, jeID)
	}
	// Journal is balanced and typed EOD_ROLLOVER, contra 4100 revenue.
	var et string
	var dSum, cSum decimal.Decimal
	if err := pool.QueryRow(ctx, `
		SELECT je.entry_type::text, COALESCE(SUM(ll.debit_amount),0), COALESCE(SUM(ll.credit_amount),0)
		FROM journal_entries je JOIN ledger_lines ll ON ll.journal_entry_id = je.id
		WHERE je.id = $1 GROUP BY je.entry_type`, *jeID).
		Scan(&et, &dSum, &cSum); err != nil {
		t.Fatalf("journal: %v", err)
	}
	if et != "EOD_ROLLOVER" || !dSum.Equal(cSum) || !dSum.Equal(decimal.RequireFromString("2.1")) {
		t.Fatalf("journal type=%s d=%s c=%s", et, dSum, cSum)
	}

	// Re-run same roll date → ALREADY_COMPLETED, no double charge.
	rep2, err := svc.RunOnce(ctx, rollNow)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if !rep2.Skipped || rep2.SkipReason != "ALREADY_COMPLETED" {
		t.Fatalf("rerun report %+v", rep2)
	}
	if bal := usdBalance(t, pool, acct); !bal.Equal(decimal.RequireFromString("2.1")) {
		t.Fatalf("after rerun balance=%s — double charge!", bal)
	}
}

// TestIntegration_RolloverWednesdayTriple — the Wednesday Fri→Mon roll
// accrues 3 calendar days (spec §17.4 item 4).
func TestIntegration_RolloverWednesdayTriple(t *testing.T) {
	svc, pool, _ := rolloverFixture(t)
	ctx := context.Background()

	// Wednesday 2025-07-16 17:00 EDT: T+2 spot date Fri 07-18 → Mon 07-21
	// = 3 days.
	rollNow := utc(t, "2025-07-16T21:00:00Z")
	seedRateAndMarkup(t, pool, "2025-07-16")

	acct := rollAccount(t, pool, "ROLLING_MARGIN", "STANDARD")
	pos := rollPosition(t, pool, acct, "100000", "1.1000", "2025-07-14T10:00:00Z")

	rep, err := svc.RunOnce(ctx, rollNow)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rep.PositionsRolled != 1 || len(rep.Errors) != 0 {
		t.Fatalf("report %+v", rep)
	}
	// 100000 × 0.000021 × 3 = 6.30 USD.
	if bal := usdBalance(t, pool, acct); !bal.Equal(decimal.RequireFromString("6.3")) {
		t.Fatalf("triple-roll balance=%s want 6.3", bal)
	}
	var days int
	var vd time.Time
	if err := pool.QueryRow(ctx,
		`SELECT days FROM swap_accrual_records WHERE position_id=$1`, pos).Scan(&days); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT value_date FROM positions WHERE id=$1`, pos).Scan(&vd); err != nil {
		t.Fatal(err)
	}
	if days != 3 || vd.Format("2006-01-02") != "2025-07-21" {
		t.Fatalf("triple roll days=%d vd=%s (want 3 / 2025-07-21)", days, vd.Format("2006-01-02"))
	}
}

// TestIntegration_PhysicalDeliveryExcluded — Task 3.3.22: a
// PHYSICAL_DELIVERY account's positions never enter the financing path.
func TestIntegration_PhysicalDeliveryExcluded(t *testing.T) {
	svc, pool, _ := rolloverFixture(t)
	ctx := context.Background()

	rollNow := utc(t, "2025-07-15T21:00:00Z")
	seedRateAndMarkup(t, pool, "2025-07-15")

	acct := rollAccount(t, pool, "PHYSICAL_DELIVERY", "STANDARD")
	pos := rollPosition(t, pool, acct, "100000", "1.1000", "2025-07-14T10:00:00Z")

	rep, err := svc.RunOnce(ctx, rollNow)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rep.PositionsScanned != 0 || rep.PositionsRolled != 0 {
		t.Fatalf("physical delivery position entered the roll: %+v", rep)
	}
	if bal := usdBalance(t, pool, acct); !bal.IsZero() {
		t.Fatalf("PHYSICAL_DELIVERY charged %s", bal)
	}
	var cnt int
	var vd *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM swap_accrual_records WHERE position_id=$1`, pos).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT value_date FROM positions WHERE id=$1`, pos).Scan(&vd); err != nil {
		t.Fatal(err)
	}
	if cnt != 0 || vd != nil {
		t.Fatalf("physical delivery touched: records=%d vd=%v", cnt, vd)
	}
}

// TestIntegration_SwapFree covers Task 3.3.23: VERIFIED accounts accrue
// exactly 0.0 swap (foregone reported on the accrual record), and
// positions held beyond the grace period assess lots ×
// daily_admin_fee_usd_per_lot posted to 4020_SWAPFREE_ADMIN_REVENUE_USD.
func TestIntegration_SwapFree(t *testing.T) {
	svc, pool, _ := rolloverFixture(t)
	ctx := context.Background()

	rollNow := utc(t, "2025-07-16T21:00:00Z")
	seedRateAndMarkup(t, pool, "2025-07-16")
	// Schedule: grace 5 days, $25 per lot per day (lot = lot_size = 1000
	// units, so qty 1000 = 1 lot).
	if _, err := pool.Exec(ctx, `
		INSERT INTO swap_free_admin_fees (instrument_id, holding_grace_days, daily_admin_fee_usd_per_lot)
		VALUES (1, 5, 25.0000)`); err != nil {
		t.Fatalf("fee schedule: %v", err)
	}

	// Position D: VERIFIED swap-free, opened 6 days before roll (past
	// grace) → zero swap + $25 admin fee.
	acctD := rollAccount(t, pool, "ROLLING_MARGIN", "VERIFIED")
	posD := rollPosition(t, pool, acctD, "1000", "1.1000", "2025-07-10T10:00:00Z")

	// Position E: VERIFIED swap-free, opened exactly 5 days before roll
	// (at the grace boundary — still free).
	acctE := rollAccount(t, pool, "ROLLING_MARGIN", "VERIFIED")
	posE := rollPosition(t, pool, acctE, "1000", "1.1000", "2025-07-11T10:00:00Z")

	rep, err := svc.RunOnce(ctx, rollNow)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rep.PositionsRolled != 2 || rep.SwapFreeZeroed != 2 || rep.FeesAssessed != 1 ||
		len(rep.Errors) != 0 {
		t.Fatalf("report %+v", rep)
	}
	if !rep.FeeTotalUSD.Equal(decimal.RequireFromString("25")) {
		t.Fatalf("fee total=%s want 25", rep.FeeTotalUSD)
	}
	// Foregone: 1000 × 0.000021 × 3 = 0.063 per position, two positions.
	if !rep.ForegoneByCcy["USD"].Equal(decimal.RequireFromString("0.126")) {
		t.Fatalf("foregone=%v", rep.ForegoneByCcy)
	}

	// D: net balance = -25 (fee debit; swap zeroed).
	if bal := usdBalance(t, pool, acctD); !bal.Equal(decimal.RequireFromString("-25")) {
		t.Fatalf("acctD balance=%s want -25", bal)
	}
	var recSwapFree bool
	var foregone, clientDelta decimal.Decimal
	var jID *int64
	if err := pool.QueryRow(ctx, `
		SELECT swap_free, foregone_amount, client_delta, journal_entry_id
		FROM swap_accrual_records WHERE position_id=$1`, posD).
		Scan(&recSwapFree, &foregone, &clientDelta, &jID); err != nil {
		t.Fatalf("accrual D: %v", err)
	}
	if !recSwapFree || !clientDelta.IsZero() || !foregone.Equal(decimal.RequireFromString("0.063")) {
		t.Fatalf("swapfree accrual: free=%v foregone=%s delta=%s", recSwapFree, foregone, clientDelta)
	}
	if jID != nil {
		t.Fatalf("swapfree accrual must carry no swap journal, got %d", *jID)
	}
	var feeAcct, feeStatus string
	var feeAmt decimal.Decimal
	if err := pool.QueryRow(ctx, `
		SELECT ll.account_code, fa.status, fa.admin_fee_amount
		FROM swap_free_admin_fee_assessments fa
		JOIN journal_entries je ON je.id = fa.journal_entry_id
		JOIN ledger_lines ll ON ll.journal_entry_id = je.id
		WHERE fa.position_id=$1 AND ll.credit_amount > 0`, posD).
		Scan(&feeAcct, &feeStatus, &feeAmt); err != nil {
		t.Fatalf("assessment D: %v", err)
	}
	if feeAcct != "4020_SWAPFREE_ADMIN_REVENUE_USD" || feeStatus != "COLLECTED" ||
		!feeAmt.Equal(decimal.RequireFromString("25")) {
		t.Fatalf("fee acct=%s status=%s amt=%s", feeAcct, feeStatus, feeAmt)
	}

	// E: exactly at grace → no fee, no balance change, but still zeroed
	// swap + accrual record.
	if bal := usdBalance(t, pool, acctE); !bal.IsZero() {
		t.Fatalf("acctE balance=%s want 0 (day 5 is inside grace)", bal)
	}
	var cntE int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM swap_free_admin_fee_assessments WHERE position_id=$1`,
		posE).Scan(&cntE); err != nil {
		t.Fatal(err)
	}
	if cntE != 0 {
		t.Fatalf("grace-boundary position assessed %d times", cntE)
	}
}

// TestIntegration_RolloverWeekend verifies Saturday/Sunday ET invocations
// are skipped.
func TestIntegration_RolloverWeekend(t *testing.T) {
	svc, _, _ := rolloverFixture(t)
	rep, err := svc.RunOnce(context.Background(), utc(t, "2025-07-19T21:00:00Z")) // Saturday
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Skipped || rep.SkipReason != "WEEKEND" {
		t.Fatalf("weekend run %+v", rep)
	}
}
