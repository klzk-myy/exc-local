// spread_offsets_pg_test.go — Task 22.3.13 PG-gated coverage: migration
// 085 schema exercise, PORTFOLIO-mode gate, Recompute persistence,
// LiveOffsets feed, maker-checker param lifecycle.
// Gated: EXC_PG_TEST=1, EXC_PG_DSN (default docker-compose :5433).
package margin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

func spreadScratch(t *testing.T, extra []string, downFile string) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("EXC_PG_TEST not set")
	}
	ctx := context.Background()
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	schema := fmt.Sprintf("spread_it_%d", time.Now().UnixNano())
	exec := func(sql string, label string) {
		t.Helper()
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
		cfg.RuntimeParams["search_path"] = schema + ",public"
		cc, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Skipf("connect: %v", err)
		}
		defer cc.Close(ctx)
		if _, err := cc.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	exec("CREATE SCHEMA "+schema, "create schema")
	for _, f := range []string{
		"001_create_instruments.up.sql",
		"002_create_users.up.sql",
		"003_create_accounts.up.sql",
		"005_create_orders.up.sql",
		"010_create_admin_audit_log.up.sql",
		"013_create_margin_accounts.up.sql",
	} {
		sql, err := os.ReadFile(filepath.Join("..", "db", "migrations", f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		exec(string(sql), f)
	}
	for _, f := range extra {
		sql, err := os.ReadFile(filepath.Join("..", "db", "migrations", f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		exec(string(sql), f)
	}
	if downFile != "" {
		sql, _ := os.ReadFile(filepath.Join("..", "db", "migrations", downFile))
		exec(string(sql), downFile)
	}
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Skipf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func spreadSeed(t *testing.T, pool *pgxpool.Pool, mode string) int64 {
	t.Helper()
	ctx := context.Background()
	var uid, aid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status) VALUES ($1,'ACTIVE') RETURNING id`,
		fmt.Sprintf("sp_%d@example.com", time.Now().UnixNano())).Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type, kyc_tier)
		 VALUES ($1,'MARGIN','T1') RETURNING id`, uid).Scan(&aid); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if mode != "" {
		if _, err := pool.Exec(ctx,
			`INSERT INTO margin_accounts (account_id, margin_mode)
			 VALUES ($1,$2)`, aid, mode); err != nil {
			t.Fatalf("seed margin account: %v", err)
		}
	}
	return aid
}

type fakeLegSource struct{ legs []OptionLeg }

func (f fakeLegSource) OpenOptionLegs(context.Context, int64) ([]OptionLeg, error) {
	return f.legs, nil
}

func TestPgSpreadOffsets_PortfolioModeGateAndPersist(t *testing.T) {
	pool := spreadScratch(t, []string{"085_option_spread_offsets.up.sql"}, "")
	ctx := context.Background()
	acct := spreadSeed(t, pool, "PORTFOLIO")
	var instrID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM instruments WHERE symbol='EUR/USD'`).Scan(&instrID); err != nil {
		t.Fatal(err)
	}

	svc, err := NewSpreadOffsetService(pool, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	legs := []OptionLeg{
		leg(11, "CALL", "LONG", "1.10", expiryA, "10", "300"),
		leg(12, "CALL", "SHORT", "1.15", expiryA, "10", "700"),
	}
	for i := range legs { // FK-real account + instrument ids
		legs[i].AccountID = acct
		legs[i].UnderlyingID = instrID
	}
	det, err := svc.Recompute(ctx, fakeLegSource{legs}, acct)
	if err != nil {
		t.Fatal(err)
	}
	if len(det.Spreads) != 1 {
		t.Fatalf("expected 1 spread, got %d", len(det.Spreads))
	}
	// Persisted as APPLIED, idempotent on rerun.
	if _, err := svc.Recompute(ctx, fakeLegSource{legs}, acct); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM option_spread_offsets
		 WHERE account_id=$1 AND status='APPLIED'`, acct).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 applied row, got %d", n)
	}
	live, err := svc.LiveOffsets(ctx, acct)
	if err != nil || len(live) != 1 {
		t.Fatalf("live offsets %v %v", live, err)
	}
	if !live[0].BoundMarginUSD.Equal(decimal.RequireFromString("500")) {
		t.Fatalf("bound %s", live[0].BoundMarginUSD)
	}
	// Break the spread (legs gone) → row BROKEN.
	if _, err := svc.Recompute(ctx, fakeLegSource{nil}, acct); err != nil {
		t.Fatal(err)
	}
	live, _ = svc.LiveOffsets(ctx, acct)
	if len(live) != 0 {
		t.Fatal("broken spread still live")
	}
}

func TestPgSpreadOffsets_NonPortfolioGetsNoOffsets(t *testing.T) {
	pool := spreadScratch(t, []string{"085_option_spread_offsets.up.sql"}, "")
	ctx := context.Background()
	acct := spreadSeed(t, pool, "CROSS")
	svc, err := NewSpreadOffsetService(pool, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	det, err := svc.Recompute(ctx, fakeLegSource{[]OptionLeg{
		leg(11, "CALL", "LONG", "1.10", expiryA, "10", "300"),
		leg(12, "CALL", "SHORT", "1.15", expiryA, "10", "700"),
	}}, acct)
	if err != nil {
		t.Fatal(err)
	}
	if len(det.Spreads) != 0 {
		t.Fatal("CROSS-mode account received offsets")
	}
}

func TestPgSpreadOffsets_MakerCheckerParams(t *testing.T) {
	pool := spreadScratch(t, []string{"085_option_spread_offsets.up.sql"}, "")
	ctx := context.Background()
	svc, err := NewSpreadOffsetService(pool, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := svc.ProposeOffset(ctx, SpreadStraddle, decimal.NewFromInt(7500), "risk.officer")
	if err != nil {
		t.Fatal(err)
	}
	// Self-approval rejected (maker-checker).
	if err := svc.Approve(ctx, id, "risk.officer"); excerrors.CodeOf(err) != CodeSpreadOffsetParamInvalid {
		t.Fatalf("self-approval: %v", err)
	}
	// Different approver activates.
	if err := svc.Approve(ctx, id, "compliance.officer"); err != nil {
		t.Fatal(err)
	}
	params, err := svc.ActiveParams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !params.OffsetsBps[SpreadStraddle].Equal(decimal.NewFromInt(7500)) {
		t.Fatalf("param %s", params.OffsetsBps[SpreadStraddle])
	}
	// Bad bps rejected.
	if _, err := svc.ProposeOffset(ctx, SpreadStraddle, decimal.NewFromInt(12000), "a"); excerrors.CodeOf(err) != CodeSpreadOffsetParamInvalid {
		t.Fatalf("bad bps: %v", err)
	}
	if _, err := svc.ProposeOffset(ctx, "BOGUS", decimal.NewFromInt(1), "a"); excerrors.CodeOf(err) != CodeSpreadOffsetParamInvalid {
		t.Fatalf("bad type: %v", err)
	}
}

func TestPgSpreadOffsets_DownMigration(t *testing.T) {
	pool := spreadScratch(t, []string{"085_option_spread_offsets.up.sql"},
		"085_option_spread_offsets.down.sql")
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_tables
		 WHERE schemaname=current_schema()
		   AND tablename IN ('option_spread_offsets','option_spread_offset_params')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("spread tables survived the down migration")
	}
}
