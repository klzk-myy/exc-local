// margin_store_test.go — PG-gated integration coverage for
// PgMarginStore (Phase-19 Task 19.3.1 store seam; migrations
// 001/003/004/013/014/042/106/110 + the uq index from 020).
//
// Same throwaway-schema convention as liquidation_store_test.go — the
// migration subset is applied verbatim, then tests seed through the
// liqSeed* helpers. Run:
//
//	EXC_PG_TEST=1 go test ./internal/risk/ -run 'TestPgMarginStore' -v
//
// (EXC_PG_DSN overrides the DSN; default is the docker-compose dev DB.)
package risk

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// marginStoreMigrations is the superset covering margin_accounts,
// balances, positions (incl. migration-106 isolated columns),
// accounts.client_category/nbp/base_currency, margin_call_events +
// admin_audit_log, and account_margin_thresholds — shared by the
// margin_call and margin_level PG fixtures too.
var marginStoreMigrations = []string{
	"001_create_instruments.up.sql",
	"002_create_users.up.sql",
	"003_create_accounts.up.sql",
	"004_create_balances.up.sql",
	"010_create_admin_audit_log.up.sql",
	"013_create_margin_accounts.up.sql",
	"014_create_positions.up.sql",
	"015_create_liquidation_auctions.up.sql",
	"016_create_insurance_fund.up.sql",
	"036_create_general_ledger.up.sql",
	"042_client_categorization.up.sql",
	"106_positions_isolated_margin.up.sql",
	"110_multi_currency_pnl.up.sql",
	"230_liquidation_risk.up.sql",
	"235_account_margin_thresholds.up.sql",
}

// marginSchemaFixture creates a scratch schema and applies the given
// migration subset — liqStoreFixture generalized for sibling stores.
func marginSchemaFixture(t *testing.T, migrations []string) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("EXC_PG_TEST not set")
	}
	dsn := liqStoreDSN()
	schema := fmt.Sprintf("margin_itest_%d", time.Now().UnixNano())
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
	boot.Close(ctx)
	t.Cleanup(func() {
		c2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel2()
		conn, err := pgx.Connect(c2, dsn)
		if err == nil {
			_, _ = conn.Exec(c2, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			conn.Close(c2)
		}
	})

	for _, m := range migrations {
		liqStoreMigExec(t, ctx, dsn, schema, "../db/migrations/"+m)
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
	return pool
}

// marginStoreFixture returns the store + pool on the shared migration
// set, with the production uq_margin_accounts_account_id uniqueness
// (migration 020) recreated inline.
func marginStoreFixture(t *testing.T) (*PgMarginStore, *pgxpool.Pool) {
	t.Helper()
	pool := marginSchemaFixture(t, marginStoreMigrations)
	if _, err := pool.Exec(context.Background(),
		`CREATE UNIQUE INDEX uq_margin_accounts_account_id ON margin_accounts (account_id)`); err != nil {
		t.Fatalf("uq index: %v", err)
	}
	st, err := NewPgMarginStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return st, pool
}

// ---------------------------------------------------------------------------
// Seed helpers local to this file
// ---------------------------------------------------------------------------

func mgnSeedBalance(t *testing.T, pool *pgxpool.Pool, acct int64, ccy, avail, locked string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO balances (account_id, currency, available, locked)
		VALUES ($1,$2,$3::numeric,$4::numeric)`, acct, ccy, avail, locked); err != nil {
		t.Fatalf("balance seed: %v", err)
	}
}

// mgnSeedInstrument inserts a SPOT instrument mirroring the migration-001
// seed shape (symbols are UNIQUE — use symbols outside the seed set).
func mgnSeedInstrument(t *testing.T, pool *pgxpool.Pool, symbol, base, quote, status string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO instruments
		    (symbol, base_currency, quote_currency, instrument_type,
		     tick_size, lot_size, min_order_qty, max_order_qty,
		     settlement_cycle, max_leverage, status)
		VALUES ($1,$2,$3,'SPOT',0.00001,1000,1000,100000000,1,30,$4::instrument_status_enum)
		RETURNING id`, symbol, base, quote, status).Scan(&id); err != nil {
		t.Fatalf("instrument seed: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// Unit (no PG)
// ---------------------------------------------------------------------------

func TestPgMarginStoreNilPool(t *testing.T) {
	if _, err := NewPgMarginStore(nil); err == nil {
		t.Fatal("nil pool must fail construction (fail-closed)")
	}
}

// ---------------------------------------------------------------------------
// margin_accounts reads/writes
// ---------------------------------------------------------------------------

func TestPgMarginStoreMarginAccount(t *testing.T) {
	st, pool := marginStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")

	// Absent row → (nil, nil) — callers resolve the §13.1 default.
	got, err := st.MarginAccount(ctx, acct)
	if err != nil || got != nil {
		t.Fatalf("absent margin account: %+v %v", got, err)
	}

	liqSeedMarginAccount(t, pool, acct, "PORTFOLIO", "MARGIN_CALL")
	if _, err := pool.Exec(ctx,
		`UPDATE margin_accounts SET equity=1234.5, used_margin=99.25 WHERE account_id=$1`,
		acct); err != nil {
		t.Fatal(err)
	}
	got, err = st.MarginAccount(ctx, acct)
	if err != nil || got == nil {
		t.Fatalf("read: %v %+v", err, got)
	}
	if got.Mode != ModePortfolio || got.Status != "MARGIN_CALL" ||
		!got.Equity.Equal(decimal.RequireFromString("1234.5")) ||
		!got.UsedMargin.Equal(decimal.RequireFromString("99.25")) {
		t.Fatalf("decode: %+v", got)
	}
}

func TestPgMarginStoreSetMarginModeUpsert(t *testing.T) {
	st, pool := marginStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")

	// First write materializes the row (status NORMAL).
	if err := st.SetMarginMode(ctx, acct, ModeIsolated); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := st.MarginAccount(ctx, acct)
	if err != nil || got == nil || got.Mode != ModeIsolated || got.Status != "NORMAL" {
		t.Fatalf("insert read-back: %v %+v", err, got)
	}
	// Conflict path flips the mode in place; one row only.
	if err := st.SetMarginMode(ctx, acct, ModePortfolio); err != nil {
		t.Fatalf("update: %v", err)
	}
	var n int
	var mode string
	if err := pool.QueryRow(ctx,
		`SELECT count(*), max(margin_mode::text) FROM margin_accounts WHERE account_id=$1`,
		acct).Scan(&n, &mode); err != nil || n != 1 || mode != "PORTFOLIO" {
		t.Fatalf("upsert: n=%d mode=%q err=%v", n, mode, err)
	}
}

func TestPgMarginStoreOpenPositionCount(t *testing.T) {
	st, pool := marginStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")
	inst := liqSeedInstrument(t, pool)

	if n, err := st.OpenPositionCount(ctx, acct); err != nil || n != 0 {
		t.Fatalf("empty count: %d %v", n, err)
	}
	liqSeedPosition(t, pool, acct, inst, "LONG", "100", "1.10", liqStr("1.10"), nil, "0", "10")
	liqSeedPosition(t, pool, acct, inst, "SHORT", "-50", "1.20", liqStr("1.20"), nil, "0", "10")
	// Flat rows never count.
	liqSeedPosition(t, pool, acct, inst, "LONG", "0", "1.10", nil, nil, "0", "0")
	if n, err := st.OpenPositionCount(ctx, acct); err != nil || n != 2 {
		t.Fatalf("count: %d %v, want 2", n, err)
	}
}

func TestPgMarginStoreAccountCategoryAndBase(t *testing.T) {
	st, pool := marginStoreFixture(t)
	ctx := context.Background()
	retail := liqSeedAccount(t, pool, "RETAIL")
	pro := liqSeedAccount(t, pool, "PROFESSIONAL")

	if c, err := st.AccountCategory(ctx, retail); err != nil || c != "RETAIL" {
		t.Fatalf("retail category: %q %v", c, err)
	}
	if c, err := st.AccountCategory(ctx, pro); err != nil || c != "PROFESSIONAL" {
		t.Fatalf("pro category: %q %v", c, err)
	}
	// Missing account is NOT_FOUND-shaped — a silent default would
	// mis-tier the account (fail closed).
	if _, err := st.AccountCategory(ctx, 4242424242); err == nil {
		t.Fatal("missing account must error")
	}

	// base_currency (migration 110) defaults USD; lower-case input
	// normalizes to upper.
	if c, err := st.AccountBaseCurrency(ctx, retail); err != nil || c != "USD" {
		t.Fatalf("default base: %q %v", c, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE accounts SET base_currency='eur' WHERE id=$1`, retail); err != nil {
		t.Fatal(err)
	}
	if c, err := st.AccountBaseCurrency(ctx, retail); err != nil || c != "EUR" {
		t.Fatalf("eur base: %q %v, want EUR", c, err)
	}
	if _, err := st.AccountBaseCurrency(ctx, 4242424242); err == nil {
		t.Fatal("missing account must error")
	}
}

func TestPgMarginStoreBalances(t *testing.T) {
	st, pool := marginStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")

	if got, err := st.Balances(ctx, acct); err != nil || len(got) != 0 {
		t.Fatalf("empty balances: %v %v", got, err)
	}
	mgnSeedBalance(t, pool, acct, "USD", "1000.5", "250.25")
	mgnSeedBalance(t, pool, acct, "eur", "88", "0") // lower-case → uppercased
	got, err := st.Balances(ctx, acct)
	if err != nil || len(got) != 2 {
		t.Fatalf("balances: %v %v", got, err)
	}
	byCcy := map[string]BalanceAmount{}
	for _, b := range got {
		byCcy[b.Currency] = b
	}
	usd, ok := byCcy["USD"]
	if !ok || !usd.Available.Equal(decimal.RequireFromString("1000.5")) ||
		!usd.Locked.Equal(decimal.RequireFromString("250.25")) ||
		!usd.Total().Equal(decimal.RequireFromString("1250.75")) {
		t.Fatalf("usd row: %+v", usd)
	}
	eur, ok := byCcy["EUR"]
	if !ok || !eur.Available.Equal(decimal.RequireFromString("88")) {
		t.Fatalf("eur row: %+v", eur)
	}
}

func TestPgMarginStoreMarginPositions(t *testing.T) {
	st, pool := marginStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")
	inst := liqSeedInstrument(t, pool) // EUR/USD: EUR base, USD quote, max_leverage 30
	liqSeedMarginAccount(t, pool, acct, "CROSS", "")

	p1 := liqSeedPosition(t, pool, acct, inst, "LONG", "1000", "1.1000",
		liqStr("1.1200"), liqStr("1.0500"), "20", "36")
	p2 := liqSeedPosition(t, pool, acct, inst, "SHORT", "-500", "1.2000",
		nil, nil, "-5", "20") // NULL mark → StoredMark nil
	if _, err := pool.Exec(ctx, `
		UPDATE positions SET isolated_margin_allocated=12.5,
		       auto_margin_replenish=TRUE WHERE id=$1`, p1); err != nil {
		t.Fatal(err)
	}
	// Flat row excluded by quantity<>0.
	liqSeedPosition(t, pool, acct, inst, "LONG", "0", "1.10", nil, nil, "0", "0")

	got, err := st.MarginPositions(ctx, acct)
	if err != nil || len(got) != 2 {
		t.Fatalf("positions: %v %+v", err, got)
	}
	p := got[0]
	if p.ID != p1 || p.Symbol != "EUR/USD" || p.Side != "LONG" ||
		p.BaseCurrency != "EUR" || p.QuoteCurrency != "USD" ||
		p.MaxLeverage != 30 || !p.Quantity.Equal(decimal.RequireFromString("1000")) ||
		!p.EntryPrice.Equal(decimal.RequireFromString("1.1")) ||
		p.StoredMark == nil || !p.StoredMark.Equal(decimal.RequireFromString("1.12")) ||
		!p.MarginUsed.Equal(decimal.RequireFromString("36")) ||
		!p.IsolatedAllocated.Equal(decimal.RequireFromString("12.5")) ||
		!p.AutoReplenish {
		t.Fatalf("row decode: %+v", p)
	}
	if got[1].ID != p2 {
		t.Fatalf("second row id %d, want %d", got[1].ID, p2)
	}
	if got[1].StoredMark != nil {
		t.Fatalf("NULL mark must decode as nil StoredMark, got %v", *got[1].StoredMark)
	}
	if got[1].AutoReplenish {
		t.Fatal("auto_margin_replenish must decode false")
	}
	// Empty account → empty slice.
	other := liqSeedAccount(t, pool, "RETAIL")
	got, err = st.MarginPositions(ctx, other)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty positions: %v %d", err, len(got))
	}
}

func TestPgMarginStoreFxPairInstruments(t *testing.T) {
	st, pool := marginStoreFixture(t)
	ctx := context.Background()

	got, err := st.FxPairInstruments(ctx, []string{"EUR", "JPY", "ZZZ"})
	if err != nil {
		t.Fatalf("pairs: %v", err)
	}
	// EUR resolves the direct EUR/USD seed; JPY the USD/JPY inverse; ZZZ
	// (no instrument) is absent — callers fail closed / disclose.
	if fp, ok := got["EUR"]; !ok || fp.Symbol != "EUR/USD" || fp.Inverted {
		t.Fatalf("EUR pair: %+v", got["EUR"])
	}
	if fp, ok := got["JPY"]; !ok || fp.Symbol != "USD/JPY" || !fp.Inverted {
		t.Fatalf("JPY pair: %+v", got["JPY"])
	}
	if _, ok := got["ZZZ"]; ok {
		t.Fatalf("uncovered currency must be absent: %+v", got["ZZZ"])
	}
	// Empty input → empty map, no query.
	got, err = st.FxPairInstruments(ctx, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty input: %v %v", got, err)
	}

	// A direct {CCY}/USD row wins over an inverse USD/{CCY} for the same
	// currency, and non-ACTIVE rows are ignored.
	mgnSeedInstrument(t, pool, "USD/EUR", "USD", "EUR", "ACTIVE")
	mgnSeedInstrument(t, pool, "QQQ/USD", "QQQ", "USD", "SUSPENDED")
	got, err = st.FxPairInstruments(ctx, []string{"EUR", "QQQ"})
	if err != nil {
		t.Fatal(err)
	}
	if fp := got["EUR"]; fp.Symbol != "EUR/USD" || fp.Inverted {
		t.Fatalf("direct must win over inverse: %+v", fp)
	}
	if _, ok := got["QQQ"]; ok {
		t.Fatalf("SUSPENDED pair must be ignored: %+v", got["QQQ"])
	}
}

func TestPgMarginStoreOpenPositionIndex(t *testing.T) {
	st, pool := marginStoreFixture(t)
	ctx := context.Background()
	a1 := liqSeedAccount(t, pool, "RETAIL")
	a2 := liqSeedAccount(t, pool, "RETAIL")
	inst := liqSeedInstrument(t, pool)
	var jpy int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM instruments WHERE symbol='USD/JPY'`).Scan(&jpy); err != nil {
		t.Fatal(err)
	}

	liqSeedPosition(t, pool, a1, inst, "LONG", "100", "1.10", nil, nil, "0", "0")
	liqSeedPosition(t, pool, a2, inst, "SHORT", "-50", "1.20", nil, nil, "0", "0")
	liqSeedPosition(t, pool, a1, jpy, "LONG", "100", "150", nil, nil, "0", "0")
	// Flat positions never index.
	liqSeedPosition(t, pool, a2, jpy, "LONG", "0", "150", nil, nil, "0", "0")

	idx, err := st.OpenPositionIndex(ctx)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if len(idx) != 2 || len(idx["EUR/USD"]) != 2 || len(idx["USD/JPY"]) != 1 {
		t.Fatalf("index shape: %+v", idx)
	}
	got := idx["EUR/USD"]
	want := []int64{a1, a2}
	if a2 < a1 {
		want = []int64{a2, a1}
	}
	if got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("EUR/USD fan-out: %v, want %v", got, want)
	}
	if idx["USD/JPY"][0] != a1 {
		t.Fatalf("USD/JPY fan-out: %v", idx["USD/JPY"])
	}
}

func TestPgMarginStoreWriteMarginSnapshot(t *testing.T) {
	st, pool := marginStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")
	liqSeedMarginAccount(t, pool, acct, "CROSS", "")

	snap := MarginSnapshot{
		AccountID: acct, Mode: ModeCross, ClientCategory: "RETAIL",
		Equity: decimal.RequireFromString("1000"), UsedMargin: decimal.RequireFromString("400"),
		AvailableMargin: decimal.RequireFromString("600"),
		Status:          "MARGIN_CALL", Ts: time.Now().UTC(),
	}
	if err := st.WriteMarginSnapshot(ctx, snap); err != nil {
		t.Fatalf("write: %v", err)
	}
	var eq, used, avail, util, status string
	if err := pool.QueryRow(ctx, `
		SELECT equity::text, used_margin::text, available_margin::text,
		       margin_utilization::text, status::text
		FROM margin_accounts WHERE account_id=$1`, acct).
		Scan(&eq, &used, &avail, &util, &status); err != nil {
		t.Fatal(err)
	}
	if !decimal.RequireFromString(eq).Equal(decimal.RequireFromString("1000")) ||
		!decimal.RequireFromString(used).Equal(decimal.RequireFromString("400")) ||
		!decimal.RequireFromString(avail).Equal(decimal.RequireFromString("600")) ||
		!decimal.RequireFromString(util).Equal(decimal.RequireFromString("0.4")) ||
		status != "MARGIN_CALL" {
		t.Fatalf("row: eq=%s used=%s avail=%s util=%s status=%s", eq, used, avail, util, status)
	}

	// Conflict path: status flips back, numbers update.
	snap.Status = "NORMAL"
	snap.Equity = decimal.RequireFromString("2000")
	snap.UsedMargin = decimal.RequireFromString("400")
	snap.AvailableMargin = decimal.RequireFromString("1600")
	if err := st.WriteMarginSnapshot(ctx, snap); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT status::text, equity::text FROM margin_accounts WHERE account_id=$1`,
		acct).Scan(&status, &eq); err != nil {
		t.Fatal(err)
	}
	if status != "NORMAL" || !decimal.RequireFromString(eq).Equal(decimal.RequireFromString("2000")) {
		t.Fatalf("updated row: %s %s", status, eq)
	}

	// Zero equity → utilization 0 (never div-by-zero), row still writes.
	acct2 := liqSeedAccount(t, pool, "RETAIL")
	if err := st.WriteMarginSnapshot(ctx, MarginSnapshot{
		AccountID: acct2, Mode: ModeCross, Status: "NORMAL"}); err != nil {
		t.Fatalf("zero-equity write: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT margin_utilization::text FROM margin_accounts WHERE account_id=$1`,
		acct2).Scan(&util); err != nil || !decimal.RequireFromString(util).IsZero() {
		t.Fatalf("zero-equity util: %q %v", util, err)
	}
}
