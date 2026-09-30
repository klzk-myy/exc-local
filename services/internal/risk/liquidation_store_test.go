// liquidation_store_test.go — unit + PG-gated integration coverage for
// PgLiquidationStore (Phase-19 Task 19.3.3 store seam; spec §5.13/§5.14,
// §13.4/§13.5, migration 015/230).
//
// The integration legs build a throwaway schema on the dev Postgres and
// apply the real migration files verbatim — the same pattern as
// internal/settlement's store tests. Run:
//
//	EXC_PG_TEST=1 go test ./internal/risk/ -run 'TestPgLiquidationStore' -v
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

// ---------------------------------------------------------------------------
// Unit (no PG)
// ---------------------------------------------------------------------------

func TestPgLiquidationStoreNilPool(t *testing.T) {
	if _, err := NewPgLiquidationStore(nil); err == nil {
		t.Fatal("nil pool must fail construction (fail-closed)")
	}
}

// ---------------------------------------------------------------------------
// Integration fixture — throwaway schema + real migration files
// ---------------------------------------------------------------------------

const liqStoreTestDSN = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"

func liqStoreDSN() string {
	if d := os.Getenv("EXC_PG_DSN"); d != "" {
		return d
	}
	return liqStoreTestDSN
}

// liqStoreMigExec applies one migration file on a simple-protocol
// connection (pgx prepared statements cannot carry BEGIN..COMMIT
// multi-statement scripts) inside the scratch schema.
func liqStoreMigExec(t *testing.T, ctx context.Context, dsn, schema, file string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	defer conn.Close(ctx)
	sql, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if _, err := conn.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply %s: %v", file, err)
	}
}

// liqStoreFixture creates the scratch schema, applies the migration
// subset the liquidation store touches, and returns the store + pool.
// The uq_margin_accounts_account_id index (owned by migration 020,
// which also indexes tables outside this subset) is recreated inline so
// margin_accounts keeps its production uniqueness contract.
func liqStoreFixture(t *testing.T) (*PgLiquidationStore, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("EXC_PG_TEST not set")
	}
	dsn := liqStoreDSN()
	schema := fmt.Sprintf("liqstore_itest_%d", time.Now().UnixNano())
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

	migDir := "../db/migrations"
	for _, m := range []string{
		"001_create_instruments.up.sql",
		"002_create_users.up.sql",
		"003_create_accounts.up.sql",
		"013_create_margin_accounts.up.sql",
		"014_create_positions.up.sql",
		"015_create_liquidation_auctions.up.sql",
		"016_create_insurance_fund.up.sql",
		"036_create_general_ledger.up.sql",
		"042_client_categorization.up.sql",
		"106_positions_isolated_margin.up.sql",
		"230_liquidation_risk.up.sql",
	} {
		liqStoreMigExec(t, ctx, dsn, schema, migDir+"/"+m)
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

	// Production uniqueness (migration 020 uq_margin_accounts_account_id)
	// recreated inside the scratch schema.
	if _, err := pool.Exec(ctx,
		`CREATE UNIQUE INDEX uq_margin_accounts_account_id ON margin_accounts (account_id)`); err != nil {
		t.Fatalf("margin_accounts uq index: %v", err)
	}

	st, err := NewPgLiquidationStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return st, pool
}

// ---------------------------------------------------------------------------
// Seed helpers
// ---------------------------------------------------------------------------

func liqSeedAccount(t *testing.T, pool *pgxpool.Pool, category string) int64 {
	t.Helper()
	ctx := context.Background()
	var uid, aid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status) VALUES ($1,'ACTIVE') RETURNING id`,
		fmt.Sprintf("liq_%d@example.com", time.Now().UnixNano())).Scan(&uid); err != nil {
		t.Fatalf("user: %v", err)
	}
	if category == "" {
		category = "RETAIL"
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO accounts (user_id, account_type, client_category)
		VALUES ($1,'MARGIN',$2::client_category_enum) RETURNING id`,
		uid, category).Scan(&aid); err != nil {
		t.Fatalf("account: %v", err)
	}
	return aid
}

func liqSeedMarginAccount(t *testing.T, pool *pgxpool.Pool, accountID int64, mode, status string) {
	t.Helper()
	if status == "" {
		status = "NORMAL"
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO margin_accounts (account_id, margin_mode, status)
		VALUES ($1, $2::margin_mode_enum, $3::margin_account_status_enum)`,
		accountID, mode, status); err != nil {
		t.Fatalf("margin account: %v", err)
	}
}

func liqSeedInstrument(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM instruments WHERE symbol='EUR/USD'`).Scan(&id); err != nil {
		t.Fatalf("seed instrument: %v", err)
	}
	return id
}

// liqSeedPosition inserts one positions row; nil pointers → NULL
// (mark_price/liquidation_price are the nullable economics).
func liqSeedPosition(t *testing.T, pool *pgxpool.Pool, accountID, instrumentID int64,
	side, qty, entry string, mark, liq *string, upnl, marginUsed string) int64 {

	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO positions
		    (account_id, instrument_id, side, quantity, entry_price,
		     mark_price, liquidation_price, unrealized_pnl, margin_used)
		VALUES ($1,$2,$3::position_side_enum,$4::numeric,$5::numeric,
		        $6::numeric,$7::numeric,$8::numeric,$9::numeric)
		RETURNING id`,
		accountID, instrumentID, side, qty, entry, mark, liq, upnl, marginUsed).
		Scan(&id); err != nil {
		t.Fatalf("position: %v", err)
	}
	return id
}

func liqStr(v string) *string { return &v }

// ---------------------------------------------------------------------------
// Position reads
// ---------------------------------------------------------------------------

func TestPgLiquidationStoreOpenPositions(t *testing.T) {
	st, pool := liqStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")
	inst := liqSeedInstrument(t, pool)
	liqSeedMarginAccount(t, pool, acct, "CROSS", "")

	// Two losers + a winner — expect ascending unrealized P&L order.
	pWin := liqSeedPosition(t, pool, acct, inst, "LONG", "1000", "1.1000",
		liqStr("1.1200"), liqStr("1.0500"), "20", "36")
	pMid := liqSeedPosition(t, pool, acct, inst, "SHORT", "500", "1.2000",
		nil, liqStr("1.2600"), "-5", "20") // NULL mark → entry fallback
	pBad := liqSeedPosition(t, pool, acct, inst, "LONG", "2000", "1.1000",
		liqStr("1.0800"), liqStr("1.0400"), "-40", "72")
	// A flat row must never appear.
	flat := liqSeedPosition(t, pool, acct, inst, "LONG", "0", "1.1000",
		liqStr("1.1000"), nil, "0", "0")
	_ = flat

	got, err := st.OpenPositions(ctx, acct)
	if err != nil {
		t.Fatalf("open positions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 open positions, got %d: %+v", len(got), got)
	}
	wantIDs := []int64{pBad, pMid, pWin}
	for i, w := range wantIDs {
		if got[i].ID != w {
			t.Fatalf("order[%d]: want pos %d, got %d", i, w, got[i].ID)
		}
	}
	// Field round-trip on the worst row.
	p := got[0]
	if p.Side != "LONG" || p.Symbol != "EUR/USD" || p.MarginMode != "CROSS" ||
		p.Quantity.String() != "2000" || p.EntryPrice.String() != "1.1" ||
		p.MarkPrice.String() != "1.08" || p.LiquidationPrice.String() != "1.04" ||
		p.UnrealizedPnl.String() != "-40" || p.MarginUsed.String() != "72" {
		t.Fatalf("row decode wrong: %+v", p)
	}
	// NULL mark fell back to entry_price on the middle row.
	if got[1].MarkPrice.String() != "1.2" {
		t.Fatalf("NULL mark must fall back to entry_price, got %s", got[1].MarkPrice)
	}
	// Empty account → empty slice, not error.
	other := liqSeedAccount(t, pool, "RETAIL")
	got, err = st.OpenPositions(ctx, other)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty account: %v %d", err, len(got))
	}
}

func TestPgLiquidationStoreOpenInterest(t *testing.T) {
	st, pool := liqStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")
	inst := liqSeedInstrument(t, pool)
	var jpy int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM instruments WHERE symbol='USD/JPY'`).Scan(&jpy); err != nil {
		t.Fatalf("jpy instrument: %v", err)
	}

	liqSeedPosition(t, pool, acct, inst, "LONG", "1000", "1.10", liqStr("1.20"), nil, "0", "0")
	liqSeedPosition(t, pool, acct, inst, "SHORT", "-500", "1.30", liqStr("1.20"), nil, "0", "0")
	// NULL mark → entry_price contribution.
	liqSeedPosition(t, pool, acct, inst, "LONG", "100", "2.00", nil, nil, "0", "0")
	// Flat + other-instrument rows must not contribute.
	liqSeedPosition(t, pool, acct, inst, "LONG", "0", "9.99", liqStr("9.99"), nil, "0", "0")
	liqSeedPosition(t, pool, acct, jpy, "LONG", "1000", "150", liqStr("151"), nil, "0", "0")

	oi, err := st.OpenInterest(ctx, inst)
	if err != nil {
		t.Fatalf("oi: %v", err)
	}
	// 1000×1.20 + 500×1.20 + 100×2.00 = 1200 + 600 + 200 = 2000.
	if got, want := oi.String(), "2000"; got != want {
		t.Fatalf("oi = %s, want %s", got, want)
	}
	// Unknown instrument → 0, not error.
	oi, err = st.OpenInterest(ctx, 999999)
	if err != nil || !oi.IsZero() {
		t.Fatalf("empty oi: %v %s", err, oi)
	}
}

func TestPgLiquidationStoreMarginMode(t *testing.T) {
	st, pool := liqStoreFixture(t)
	ctx := context.Background()

	cross := liqSeedAccount(t, pool, "RETAIL")
	liqSeedMarginAccount(t, pool, cross, "CROSS", "")
	if m, err := st.MarginMode(ctx, cross); err != nil || m != "CROSS" {
		t.Fatalf("cross mode: %q %v", m, err)
	}
	// No margin row → §13.1 category default (ModeFor mirror).
	retailNoRow := liqSeedAccount(t, pool, "RETAIL")
	if m, err := st.MarginMode(ctx, retailNoRow); err != nil || m != "CROSS" {
		t.Fatalf("retail default: %q %v", m, err)
	}
	profNoRow := liqSeedAccount(t, pool, "PROFESSIONAL")
	if m, err := st.MarginMode(ctx, profNoRow); err != nil || m != "PORTFOLIO" {
		t.Fatalf("professional default: %q %v", m, err)
	}
	if _, err := st.MarginMode(ctx, 4242424242); err == nil {
		t.Fatal("missing account must fail closed")
	}
}

func TestPgLiquidationStoreSetMarginAccountStatus(t *testing.T) {
	st, pool := liqStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")
	liqSeedMarginAccount(t, pool, acct, "CROSS", "")

	if err := st.SetMarginAccountStatus(ctx, acct, "LIQUIDATING"); err != nil {
		t.Fatalf("set status: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM margin_accounts WHERE account_id=$1`,
		acct).Scan(&status); err != nil || status != "LIQUIDATING" {
		t.Fatalf("status read-back: %q %v", status, err)
	}
	// Missing row → NOT_FOUND-shaped error, never silent.
	if err := st.SetMarginAccountStatus(ctx, 9876543210, "LIQUIDATING"); err == nil {
		t.Fatal("status update on missing margin row must error")
	}
	// Invalid enum value → PG error surfaces.
	if err := st.SetMarginAccountStatus(ctx, acct, "BOGUS"); err == nil {
		t.Fatal("invalid status must error")
	}
}

func TestPgLiquidationStoreAccountsForScan(t *testing.T) {
	st, pool := liqStoreFixture(t)
	ctx := context.Background()
	c1 := liqSeedAccount(t, pool, "RETAIL")
	c2 := liqSeedAccount(t, pool, "RETAIL")
	iso := liqSeedAccount(t, pool, "RETAIL")
	pf := liqSeedAccount(t, pool, "PROFESSIONAL")
	liqSeedMarginAccount(t, pool, c1, "CROSS", "")
	liqSeedMarginAccount(t, pool, c2, "CROSS", "LIQUIDATING")
	liqSeedMarginAccount(t, pool, iso, "ISOLATED", "")
	liqSeedMarginAccount(t, pool, pf, "PORTFOLIO", "")

	got, err := st.AccountsForScan(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	want := map[int64]bool{c1: true, c2: true, pf: true}
	if len(got) != len(want) {
		t.Fatalf("scan ids %v, want %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("unexpected id %d in scan set", id)
		}
	}
}

func TestPgLiquidationStoreIsolatedBreaches(t *testing.T) {
	st, pool := liqStoreFixture(t)
	ctx := context.Background()
	iso := liqSeedAccount(t, pool, "RETAIL")
	cross := liqSeedAccount(t, pool, "RETAIL")
	inst := liqSeedInstrument(t, pool)
	liqSeedMarginAccount(t, pool, iso, "ISOLATED", "")
	liqSeedMarginAccount(t, pool, cross, "CROSS", "")

	longBreach := liqSeedPosition(t, pool, iso, inst, "LONG", "100", "1.10",
		liqStr("1.04"), liqStr("1.05"), "-6", "4") // mark 1.04 ≤ liq 1.05
	shortBreach := liqSeedPosition(t, pool, iso, inst, "SHORT", "-50", "1.10",
		liqStr("1.16"), liqStr("1.15"), "-3", "2") // mark 1.16 ≥ liq 1.15
	// Healthy isolated (mark not crossed) and NULL-liquidation rows — out.
	liqSeedPosition(t, pool, iso, inst, "LONG", "100", "1.10",
		liqStr("1.08"), liqStr("1.05"), "-2", "4")
	liqSeedPosition(t, pool, iso, inst, "LONG", "100", "1.10",
		liqStr("0.50"), nil, "-60", "4")
	// Breached but CROSS mode — handled by the account-level path.
	liqSeedPosition(t, pool, cross, inst, "LONG", "100", "1.10",
		liqStr("0.90"), liqStr("1.00"), "-20", "4")

	got, err := st.IsolatedBreaches(ctx)
	if err != nil {
		t.Fatalf("breaches: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 breached isolated positions, got %d: %+v", len(got), got)
	}
	if got[0].ID != longBreach || got[1].ID != shortBreach {
		t.Fatalf("breach ids: %+v", got)
	}
	if got[0].MarginMode != "ISOLATED" {
		t.Fatalf("mode: %s", got[0].MarginMode)
	}
}

func TestPgLiquidationStorePositionByID(t *testing.T) {
	st, pool := liqStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")
	inst := liqSeedInstrument(t, pool)
	liqSeedMarginAccount(t, pool, acct, "ISOLATED", "")
	open := liqSeedPosition(t, pool, acct, inst, "LONG", "100", "1.10",
		liqStr("1.08"), liqStr("1.05"), "-2", "4")

	p, err := st.PositionByID(ctx, open)
	if err != nil || p == nil {
		t.Fatalf("open position: %v %+v", err, p)
	}
	if p.Symbol != "EUR/USD" || p.MarkPrice.String() != "1.08" ||
		p.LiquidationPrice.String() != "1.05" || p.MarginMode != "ISOLATED" {
		t.Fatalf("decode: %+v", p)
	}
	// Settled (quantity zeroed) reads as gone.
	if _, err := pool.Exec(ctx, `UPDATE positions SET quantity=0 WHERE id=$1`, open); err != nil {
		t.Fatal(err)
	}
	p, err = st.PositionByID(ctx, open)
	if err != nil || p != nil {
		t.Fatalf("settled position must read nil,nil: %v %+v", err, p)
	}
	p, err = st.PositionByID(ctx, 31337)
	if err != nil || p != nil {
		t.Fatalf("missing position must read nil,nil: %v %+v", err, p)
	}
}

// ---------------------------------------------------------------------------
// liquidation_events + MarkPositionClosed
// ---------------------------------------------------------------------------

func TestPgLiquidationStoreRecordLiquidationEvent(t *testing.T) {
	st, pool := liqStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")
	inst := liqSeedInstrument(t, pool)
	liqSeedMarginAccount(t, pool, acct, "CROSS", "")
	pos := liqSeedPosition(t, pool, acct, inst, "LONG", "100", "1.10",
		liqStr("1.08"), liqStr("1.05"), "-2", "4")

	q := int(4)
	id, err := st.RecordLiquidationEvent(ctx, LiquidationEventRow{
		AccountID: acct, PositionID: pos, InstrumentID: inst,
		Kind: "ADL", Side: "LONG",
		Quantity:                  decimal.NewFromInt(100),
		Price:                     decimal.RequireFromString("1.075"),
		MarkPrice:                 decimal.RequireFromString("1.08"),
		InsuranceFundContribution: decimal.RequireFromString("-1.25"),
		PenaltyAmount:             decimal.RequireFromString("0.5"),
		ADLQuintile:               &q,
	})
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if id <= 0 {
		t.Fatalf("bad id %d", id)
	}
	var kind, side, qty, price, ifc string
	var quint *int
	if err := pool.QueryRow(ctx, `
		SELECT kind, side::text, quantity::text, price::text,
		       insurance_fund_contribution::text, adl_quintile
		FROM liquidation_events WHERE id=$1`, id).
		Scan(&kind, &side, &qty, &price, &ifc, &quint); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if kind != "ADL" || side != "LONG" ||
		!decimal.RequireFromString(qty).Equal(decimal.NewFromInt(100)) ||
		!decimal.RequireFromString(price).Equal(decimal.RequireFromString("1.075")) ||
		!decimal.RequireFromString(ifc).Equal(decimal.RequireFromString("-1.25")) ||
		quint == nil || *quint != 4 {
		t.Fatalf("event round-trip: %s %s %s %s %s %v", kind, side, qty, price, ifc, quint)
	}
	// CHECK violation on kind surfaces as an error (fail closed).
	if _, err := st.RecordLiquidationEvent(ctx, LiquidationEventRow{
		AccountID: acct, PositionID: pos, InstrumentID: inst,
		Kind: "BOGUS", Side: "LONG",
		Quantity: decimal.One, Price: decimal.One, MarkPrice: decimal.One,
	}); err == nil {
		t.Fatal("invalid kind must error")
	}
}

func TestPgLiquidationStoreMarkPositionClosed(t *testing.T) {
	st, pool := liqStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")
	inst := liqSeedInstrument(t, pool)
	liqSeedMarginAccount(t, pool, acct, "ISOLATED", "")
	pos := liqSeedPosition(t, pool, acct, inst, "LONG", "100", "1.10",
		liqStr("1.08"), liqStr("1.05"), "-2", "4")
	if _, err := pool.Exec(ctx,
		`UPDATE positions SET isolated_margin_allocated=3.5, auto_margin_replenish=TRUE,
		        realized_pnl=0.25 WHERE id=$1`, pos); err != nil {
		t.Fatal(err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkPositionClosed(ctx, tx, pos,
		decimal.RequireFromString("1.07"), decimal.RequireFromString("-3")); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var qty, upnl, rpnl, mark, marginUsed, iso string
	var liq *string
	var repl bool
	if err := pool.QueryRow(ctx, `
		SELECT quantity::text, unrealized_pnl::text, realized_pnl::text,
		       mark_price::text, margin_used::text, isolated_margin_allocated::text,
		       liquidation_price::text, auto_margin_replenish
		FROM positions WHERE id=$1`, pos).
		Scan(&qty, &upnl, &rpnl, &mark, &marginUsed, &iso, &liq, &repl); err != nil {
		t.Fatal(err)
	}
	eq := func(got, want string) bool {
		return decimal.RequireFromString(got).Equal(decimal.RequireFromString(want))
	}
	if !eq(qty, "0") || !eq(upnl, "0") || !eq(rpnl, "-2.75") || !eq(mark, "1.07") ||
		!eq(marginUsed, "0") || !eq(iso, "0") || liq != nil || repl {
		t.Fatalf("closed row: qty=%s upnl=%s rpnl=%s mark=%s mu=%s iso=%s liq=%v repl=%v",
			qty, upnl, rpnl, mark, marginUsed, iso, liq, repl)
	}
	// Second close on the flat row → error, tx rolls back.
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkPositionClosed(ctx, tx, pos,
		decimal.RequireFromString("1.07"), decimal.Zero); err == nil {
		t.Fatal("re-close of a flat position must error")
	}
	_ = tx.Rollback(ctx)
}

// ---------------------------------------------------------------------------
// Auction ladder persistence
// ---------------------------------------------------------------------------

func TestPgLiquidationStoreAuctionLifecycle(t *testing.T) {
	st, pool := liqStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")
	inst := liqSeedInstrument(t, pool)
	pos := liqSeedPosition(t, pool, acct, inst, "LONG", "10000", "1.10",
		liqStr("1.08"), liqStr("1.05"), "-20", "40")

	now := time.Now().UTC().Truncate(time.Microsecond)
	id, err := st.InsertAuction(ctx, AuctionRow{
		InstrumentID: inst, PositionID: pos, Phase: AuctionPhaseCall,
		FloorPrice:   decimal.RequireFromString("1.029"),
		UnfilledQty:  decimal.NewFromInt(10000),
		PhaseStartAt: now, PhaseEndAt: now.Add(5 * time.Second), CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("insert auction: %v", err)
	}
	row, err := st.AuctionByID(ctx, id)
	if err != nil || row == nil {
		t.Fatalf("auction read: %v %+v", err, row)
	}
	if row.Phase != AuctionPhaseCall || row.FloorPrice.String() != "1.029" ||
		row.UnfilledQty.String() != "10000" || !row.FilledQty.IsZero() ||
		!row.AvgFillPrice.IsZero() || !row.PhaseStartAt.Equal(now) ||
		!row.PhaseEndAt.Equal(now.Add(5*time.Second)) || !row.CreatedAt.Equal(now) {
		t.Fatalf("auction round-trip: %+v", row)
	}
	if row.OriginalQty().String() != "10000" {
		t.Fatalf("original qty: %s", row.OriginalQty())
	}
	// Missing id → nil,nil.
	if row, err = st.AuctionByID(ctx, 777777); err != nil || row != nil {
		t.Fatalf("missing auction: %v %+v", err, row)
	}

	// EXTEND the floor.
	extEnd := now.Add(65 * time.Second)
	if err := st.UpdateAuctionPhase(ctx, id, AuctionPhaseExtend,
		decimal.RequireFromString("1.023"), now.Add(5*time.Second), extEnd); err != nil {
		t.Fatalf("extend: %v", err)
	}
	row, _ = st.AuctionByID(ctx, id)
	if row.Phase != AuctionPhaseExtend || row.FloorPrice.String() != "1.023" ||
		!row.PhaseEndAt.Equal(extEnd) {
		t.Fatalf("post-extend: %+v", row)
	}
	// Phase update on a missing id errors.
	if err := st.UpdateAuctionPhase(ctx, 777777, AuctionPhaseExtend,
		decimal.One, now, extEnd); err == nil {
		t.Fatal("phase update on missing auction must error")
	}

	// Monotonic fill: first partial fill lands; a replayed lower state is
	// a no-op; a larger state lands.
	if err := st.RecordAuctionFill(ctx, id, decimal.NewFromInt(4000),
		decimal.RequireFromString("1.028"), decimal.NewFromInt(6000)); err != nil {
		t.Fatalf("fill 1: %v", err)
	}
	if err := st.RecordAuctionFill(ctx, id, decimal.NewFromInt(2000),
		decimal.RequireFromString("1.000"), decimal.NewFromInt(8000)); err != nil {
		t.Fatalf("replay must be a no-op, got %v", err)
	}
	row, _ = st.AuctionByID(ctx, id)
	if row.FilledQty.String() != "4000" || row.UnfilledQty.String() != "6000" ||
		row.AvgFillPrice.String() != "1.028" {
		t.Fatalf("replay overwrote newer state: %+v", row)
	}
	if err := st.RecordAuctionFill(ctx, id, decimal.NewFromInt(10000),
		decimal.RequireFromString("1.026"), decimal.Zero); err != nil {
		t.Fatalf("fill 2: %v", err)
	}
	row, _ = st.AuctionByID(ctx, id)
	if row.FilledQty.String() != "10000" || !row.UnfilledQty.IsZero() {
		t.Fatalf("terminal fill: %+v", row)
	}
	// Fill on a missing id errors.
	if err := st.RecordAuctionFill(ctx, 777777, decimal.One, decimal.One, decimal.Zero); err == nil {
		t.Fatal("fill on missing auction must error")
	}
}

func TestPgLiquidationStoreActiveAuctions(t *testing.T) {
	st, pool := liqStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")
	inst := liqSeedInstrument(t, pool)
	pos := liqSeedPosition(t, pool, acct, inst, "LONG", "10", "1", liqStr("1"), nil, "0", "0")
	now := time.Now().UTC()

	// Live CALL (window open), live EXTEND past window edge but unfilled
	// (force-cash retry must keep seeing it), and a completed row whose
	// window already lapsed.
	live, err := st.InsertAuction(ctx, AuctionRow{
		InstrumentID: inst, PositionID: pos, Phase: AuctionPhaseCall,
		FloorPrice: decimal.One, UnfilledQty: decimal.NewFromInt(10),
		PhaseStartAt: now, PhaseEndAt: now.Add(5 * time.Second), CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	pastDeadline, err := st.InsertAuction(ctx, AuctionRow{
		InstrumentID: inst, PositionID: pos, Phase: AuctionPhaseForceCash,
		FloorPrice: decimal.One, UnfilledQty: decimal.NewFromInt(5),
		PhaseStartAt: now.Add(-70 * time.Second), PhaseEndAt: now.Add(-5 * time.Second),
		CreatedAt: now.Add(-70 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	done, err := st.InsertAuction(ctx, AuctionRow{
		InstrumentID: inst, PositionID: pos, Phase: AuctionPhaseFill,
		FloorPrice: decimal.One, UnfilledQty: decimal.Zero,
		FilledQty: decimal.NewFromInt(7), AvgFillPrice: decimal.One,
		PhaseStartAt: now.Add(-70 * time.Second), PhaseEndAt: now.Add(-5 * time.Second),
		CreatedAt: now.Add(-70 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.ActiveAuctions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]bool{}
	for _, r := range got {
		ids[r.ID] = true
	}
	if !ids[live] || !ids[pastDeadline] {
		t.Fatalf("live auctions missing: %v (live=%d past=%d)", ids, live, pastDeadline)
	}
	if ids[done] {
		t.Fatalf("terminal auction %d must not be active", done)
	}

	// A completed row whose window has NOT lapsed still appears — the
	// advancer's complete() path must be able to heal a crashed retire.
	heal, err := st.InsertAuction(ctx, AuctionRow{
		InstrumentID: inst, PositionID: pos, Phase: AuctionPhaseFill,
		FloorPrice: decimal.One, UnfilledQty: decimal.Zero,
		FilledQty: decimal.NewFromInt(3), AvgFillPrice: decimal.One,
		PhaseStartAt: now, PhaseEndAt: now.Add(30 * time.Second), CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = st.ActiveAuctions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range got {
		if r.ID == heal {
			found = true
		}
	}
	if !found {
		t.Fatal("freshly completed auction inside its window must stay scannable")
	}
}
