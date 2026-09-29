// EXC_PG_TEST=1 gated integration tests — migration 045 (mm_programs +
// mm_compliance + mm_rebate_accruals) and the PgStore compliance
// rollup path over a scratch schema. Same convention as
// internal/auth/apikey_test.go.
//
//	Run: EXC_PG_TEST=1 go test ./internal/marketmaking -v
//	PG:  EXC_PG_DSN or postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable
package marketmaking

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func mmTestDSN() string {
	if d := os.Getenv("EXC_PG_DSN"); d != "" {
		return d
	}
	return "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
}

// itestMM applies migrations 001/002/003/045 into a scratch schema,
// proves 045 down + re-up, and returns a schema-bound pool plus a
// fixture account + the seeded EURUSD instrument id.
func itestMM(t *testing.T) (*pgxpool.Pool, int64, int64) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres integration tests")
	}
	dsn := mmTestDSN()
	schema := fmt.Sprintf("mm_itest_%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		conn.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	apply := func(file string) {
		t.Helper()
		sql, err := os.ReadFile("../db/migrations/" + file)
		if err != nil {
			conn.Close(ctx)
			t.Fatalf("read %s: %v", file, err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			conn.Close(ctx)
			t.Fatalf("apply %s: %v", file, err)
		}
	}
	for _, f := range []string{
		"001_create_instruments.up.sql", "002_create_users.up.sql",
		"003_create_accounts.up.sql", "045_market_maker_program.up.sql",
	} {
		apply(f)
	}
	// Rollback proof: down then re-up.
	apply("045_market_maker_program.down.sql")
	apply("045_market_maker_program.up.sql")

	// Fixtures: one user + SPOT account + the migration-001 seeded EURUSD.
	var userID, accountID, instID int64
	if err := conn.QueryRow(ctx,
		`INSERT INTO users (email, kyc_status) VALUES ('mm-itest@ex.test','VERIFIED')
		 RETURNING id`).Scan(&userID); err != nil {
		conn.Close(ctx)
		t.Fatalf("user fixture: %v", err)
	}
	if err := conn.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type) VALUES ($1,'SPOT')
		 RETURNING id`, userID).Scan(&accountID); err != nil {
		conn.Close(ctx)
		t.Fatalf("account fixture: %v", err)
	}
	if err := conn.QueryRow(ctx,
		`SELECT id FROM instruments WHERE symbol='EUR/USD'`).Scan(&instID); err != nil {
		conn.Close(ctx)
		t.Fatalf("instrument fixture: %v", err)
	}
	conn.Close(ctx)

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pool dsn: %v", err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		c2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		bare, err := pgx.ParseConfig(dsn)
		if err != nil {
			return
		}
		c, err := pgx.ConnectConfig(c2, bare)
		if err == nil {
			_, _ = c.Exec(c2, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			c.Close(c2)
		}
	})
	return pool, accountID, instID
}

// TestPgProgramRoundTripAndRollup exercises the full PgStore surface:
// enroll → entitlement → obligation sampling → daily rollup verdict →
// breach tracking → suspension.
func TestPgProgramRoundTripAndRollup(t *testing.T) {
	pool, accountID, instID := itestMM(t)
	ctx := context.Background()
	svc := NewService(NewPgStore(pool), Options{})

	p := &Program{
		AccountID:    accountID,
		InstrumentID: &instID,
		MinQuoteSize: mustDec(t, "100000"),
		MaxSpreadBps: mustDec(t, "5"),
		PresencePct:  mustDec(t, "50"),
		MMPMaxFills:  10,
		MMPWindowMs:  1000,
		RebateBps:    mustDec(t, "0.5"),
	}
	if err := svc.Enroll(ctx, p); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if p.ID == 0 || p.Status != StatusActive {
		t.Fatalf("enroll must persist ACTIVE row: %+v", p)
	}
	// Unique (account, instrument): duplicate enroll fails.
	dup := *p
	dup.ID = 0
	if err := svc.Enroll(ctx, &dup); err == nil {
		t.Fatal("duplicate (account,instrument) program must fail")
	}
	// Program-wide row (instrument NULL) is a second, legal row.
	wide := &Program{
		AccountID:    accountID,
		MinQuoteSize: mustDec(t, "50000"),
		MaxSpreadBps: mustDec(t, "8"),
		PresencePct:  mustDec(t, "50"),
		MMPMaxFills:  5, MMPWindowMs: 500,
		RebateBps: mustDec(t, "0.25"),
	}
	if err := svc.Enroll(ctx, wide); err != nil {
		t.Fatalf("program-wide enroll: %v", err)
	}
	// Entitled prefers the per-instrument row.
	got, err := svc.Entitled(ctx, accountID, instID)
	if err != nil || got == nil || got.ID != p.ID {
		t.Fatalf("specific entitlement: %+v %v", got, err)
	}
	// OTR allowance snapshot honors per-instrument > program-wide.
	otr := mustDec(t, "2000")
	upd := *wide
	upd.OtrAllowance = &otr
	if err := svc.Update(ctx, &upd); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := svc.Load(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	instSym, _ := svc.store.InstrumentSymbol(ctx, instID)
	if a := svc.OtrAllowance(ctx, accountID, instSym); a == nil ||
		!a.Equal(mustDec(t, "2000")) {
		t.Fatalf("otr allowance: %v", a)
	}
	// Obligation sampling → rollup breach (1/4 compliant = 25% < 50%).
	day := time.Now().UTC().Truncate(24 * time.Hour)
	for i := 0; i < 4; i++ {
		if err := svc.store.RecordSample(ctx, p.ID, day, i == 0); err != nil {
			t.Fatalf("record sample: %v", err)
		}
	}
	row, err := svc.RollupDay(ctx, p.ID, day)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if !row.Breach || row.PresencePct == nil || !row.PresencePct.Equal(mustDec(t, "25")) {
		t.Fatalf("rollup verdict wrong: %+v", row)
	}
	// Suspend → entitlement drops, rebate accrual pauses.
	if err := svc.Suspend(ctx, p.ID, "itest"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	got, _ = svc.Entitled(ctx, accountID, instID)
	if got == nil || got.ID != wide.ID {
		t.Fatalf("suspended specific row must fall back to wide: %+v", got)
	}
	amt, err := svc.AccrueRebate(ctx, accountID, instID, "pg-fill-1",
		mustDec(t, "100000"), mustDec(t, "1.1"))
	if err != nil {
		t.Fatalf("accrue (wide covers): %v", err)
	}
	// Wide program accrues: 100000 × 1.1 × 0.25/10000 = 2.75 USD.
	if !amt.Equal(mustDec(t, "2.75")) {
		t.Fatalf("wide-program rebate math: %s", amt)
	}
	// Idempotent fill_ref replay is a no-op.
	if _, err := svc.AccrueRebate(ctx, accountID, instID, "pg-fill-1",
		mustDec(t, "100000"), mustDec(t, "1.1")); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.Rebates(ctx, wide.ID, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rebate dedup: %d rows %v", len(rows), err)
	}
}
