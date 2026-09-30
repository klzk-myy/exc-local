// variation_margin_pg_test.go — Task 22.3.7 PG-gated coverage.
// Gated: EXC_PG_TEST=1. Applies migrations into a scratch schema on the
// dev database (EXC_PG_DSN, default docker-compose :5433) — simple
// protocol per the multi-statement scripts convention.
package risk

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
)

func vmMigExec(t *testing.T, ctx context.Context, dsn, schema, file string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	sql, err := os.ReadFile(filepath.Join("..", "db", "migrations", file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if _, err := conn.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply %s: %v", file, err)
	}
}

func vmDSN() string {
	if d := os.Getenv("EXC_PG_DSN"); d != "" {
		return d
	}
	return "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
}

// vmScratch applies the dependency chain + migration 034 into a fresh
// schema and returns a search_path'd pool.
func vmScratch(t *testing.T, up bool) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("EXC_PG_TEST not set")
	}
	ctx := context.Background()
	dsn := vmDSN()
	schema := fmt.Sprintf("vm_it_%d", time.Now().UnixNano())
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Skipf("parse dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		conn.Close(ctx)
		t.Skipf("create schema: %v", err)
	}
	conn.Close(ctx)
	files := []string{
		"001_create_instruments.up.sql",
		"002_create_users.up.sql",
		"003_create_accounts.up.sql",
		"014_create_positions.up.sql",
		"036_create_general_ledger.up.sql",
		"034_create_variation_margin.up.sql",
	}
	if !up { // down-migration probe
		files = append(files, "034_create_variation_margin.down.sql")
	}
	for _, f := range files {
		vmMigExec(t, ctx, dsn, schema, f)
	}
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pool cfg: %v", err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Skipf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func vmSeed(t *testing.T, pool *pgxpool.Pool) (acctID, posID int64) {
	t.Helper()
	ctx := context.Background()
	var uid, iid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status) VALUES ($1,'ACTIVE') RETURNING id`,
		fmt.Sprintf("vm_%d@example.com", time.Now().UnixNano())).Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type, kyc_tier)
		 VALUES ($1,'MARGIN','T1') RETURNING id`, uid).Scan(&acctID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO instruments (symbol, base_currency, quote_currency,
		    instrument_type, tick_size, lot_size, min_order_qty, max_order_qty,
		    settlement_cycle, max_leverage, status)
		VALUES ('EUR/USD-F','EUR','USD','FORWARD',0.0001,1000,1,1e8,1,10,'ACTIVE')
		RETURNING id`).Scan(&iid); err != nil {
		t.Fatalf("seed instrument: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO positions (account_id, instrument_id, side, quantity,
		    entry_price, mark_price)
		VALUES ($1,$2,'LONG',1000,1.1000,1.1500) RETURNING id`,
		acctID, iid).Scan(&posID); err != nil {
		t.Fatalf("seed position: %v", err)
	}
	return acctID, posID
}

// pgJournalPoster is the PG test's VMPoster: it validates the journal
// shape, writes a real journal_entries row (so the vm row's FK resolves)
// and returns its id — the ledger-service plumbing itself is covered by
// the settlement package's own tests.
type pgJournalPoster struct {
	pool   *pgxpool.Pool
	posted []ledger.Journal
}

func (p *pgJournalPoster) Post(ctx context.Context, j ledger.Journal) (ledger.PostResult, error) {
	if err := j.Validate(); err != nil {
		return ledger.PostResult{}, err
	}
	p.posted = append(p.posted, j)
	var id int64
	err := p.pool.QueryRow(ctx, `
		INSERT INTO journal_entries (entry_type, reference_id, description,
		    posted_by, idempotency_key)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id`, string(j.EntryType), j.ReferenceID, j.Description,
		j.PostedBy, j.IdempotencyKey).Scan(&id)
	if err != nil {
		return ledger.PostResult{}, err
	}
	return ledger.PostResult{JournalID: id, Committed: true}, nil
}

func TestPgVariationMargin_SweepAndIdempotency(t *testing.T) {
	pool := vmScratch(t, true)
	ctx := context.Background()
	_, posID := vmSeed(t, pool)

	store := &PgVMStore{Pool: pool}
	poster := &pgJournalPoster{pool: pool}
	svc, err := NewVariationMarginService(store, poster, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Truncate(24 * time.Hour)

	rep, err := svc.Sweep(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	// MTM = 1000 × (1.15 − 1.10) = 50.
	if rep.Settled != 1 || rep.Posted != 1 {
		t.Fatalf("report %+v", rep)
	}
	if len(poster.posted) != 1 {
		t.Fatal("no journal")
	}
	if !poster.posted[0].Effects[0].AvailableDelta.Equal(decimal.RequireFromString("50")) {
		t.Fatalf("vm %s want 50", poster.posted[0].Effects[0].AvailableDelta)
	}
	// Row settled.
	var settledAt *time.Time
	var vmAmt, mtm decimal.Decimal
	if err := pool.QueryRow(ctx, `
		SELECT vm_amount, mtm_value, settled_at FROM variation_margin
		WHERE subject_ref=$1`, posID).Scan(&vmAmt, &mtm, &settledAt); err != nil {
		t.Fatal(err)
	}
	if settledAt == nil || !vmAmt.Equal(decimal.RequireFromString("50")) {
		t.Fatalf("row vm=%s settled_at=%v", vmAmt, settledAt)
	}
	// Same-day rerun → skipped, no second post.
	rep, err = svc.Sweep(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 1 || rep.Posted != 0 || len(poster.posted) != 1 {
		t.Fatalf("rerun posted: %+v", rep)
	}
	// Next day: mark moves to 1.16 → MTM 60 → VM 10.
	if _, err := pool.Exec(ctx,
		`UPDATE positions SET mark_price=1.16 WHERE id=$1`, posID); err != nil {
		t.Fatal(err)
	}
	rep, err = svc.Sweep(ctx, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Settled != 1 || len(poster.posted) != 2 {
		t.Fatalf("day2 report %+v", rep)
	}
	if !poster.posted[1].Effects[0].AvailableDelta.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("day2 vm %s want 10", poster.posted[1].Effects[0].AvailableDelta)
	}
}

func TestPgVariationMargin_DownMigration(t *testing.T) {
	pool := vmScratch(t, false) // applies 034 down too
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_tables
		 WHERE schemaname=current_schema() AND tablename='variation_margin'`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("variation_margin survived the down migration")
	}
}
