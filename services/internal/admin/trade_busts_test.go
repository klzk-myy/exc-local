// Tests for the Phase-15 Task 15.3.5 trade bust / price-adjust workflow
// and the Task 15.3.8 instrument maintenance workflow.
//
// Pure unit tests cover the journal builders (zero-sum, fee refund,
// delivery unwind), symbol/param validation, session-boundary math and
// role gates. Stateful flows run against dev Postgres (EXC_PG_TEST=1 →
// scratch schema via search_path; the REAL migrations 051/219/090 are
// applied verbatim so the deliverable DDL is the thing under test — the
// pre-existing dependency tables are fixture-mirrored, same discipline
// as settlement's ledgerEntriesFixture).
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Fakes — the service seams exercised without infrastructure.
// ---------------------------------------------------------------------------

type bustFakePoster struct {
	journals []ledger.Journal
	results  []ledger.PostResult
	err      error
}

func (f *bustFakePoster) PostJournal(_ context.Context, _ pgx.Tx,
	j ledger.Journal) (ledger.PostResult, error) {
	if err := j.Validate(); err != nil {
		return ledger.PostResult{}, err // posters must refuse unbalanced journals
	}
	f.journals = append(f.journals, j)
	var events []ledger.BalanceEvent
	for _, e := range j.Effects {
		events = append(events, ledger.BalanceEvent{
			AccountID: e.AccountID, Currency: e.Currency,
			JournalID: int64(len(f.journals)), LedgerEntryID: int64(len(f.journals)),
			Available: e.AvailableDelta.String(), EventType: "BALANCE_CHANGED",
		})
	}
	res := ledger.PostResult{JournalID: int64(len(f.journals)),
		Events: events, Committed: true}
	f.results = append(f.results, res)
	return res, f.err
}

type bustFakeLocker struct{ held []string }

func (f *bustFakeLocker) TryLockAccount(_ context.Context, accountID, _ string,
	_ time.Duration) (bool, error) {
	f.held = append(f.held, accountID)
	return true, nil
}
func (f *bustFakeLocker) UnlockAccount(_ context.Context, accountID, _ string) (bool, error) {
	return true, nil
}

type bustFakePub struct {
	subjects []string
	err      error
}

func (f *bustFakePub) Publish(_ context.Context, subject string, _ []byte) error {
	if f.err != nil {
		return f.err
	}
	f.subjects = append(f.subjects, subject)
	return nil
}

type bustFakeNotifier struct {
	events []string
	users  []int64
}

func (f *bustFakeNotifier) Notify(_ context.Context, userID int64, event string,
	_ map[string]any) (int, error) {
	f.events = append(f.events, event)
	f.users = append(f.users, userID)
	return 1, nil
}

func testRoles(roles map[int64]string) AdminRoleResolver {
	return func(_ context.Context, id int64) (string, error) {
		if r, ok := roles[id]; ok {
			return r, nil
		}
		return "", fmt.Errorf("no binding")
	}
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// testBustTrade is the canonical fixture trade: EUR/USD, qty 1000 @ 1.10
// (market ref 1.00 — a 10% deviation outside the 2×5% band), fees 2/3.
func testBustTrade() *tradeRow {
	return &tradeRow{
		id: 42, instrument: 7, symbol: "EUR/USD", base: "EUR", quote: "USD",
		buyOrder: 11, sellOrder: 12, buyer: 101, seller: 102,
		price: dec("1.10"), qty: dec("1000"),
		buyerFee: dec("2"), sellerFee: dec("3"),
		intent: "ROLLING_MARGIN", status: TradeStatusCompleted,
		createdAt: time.Now(), bandPct: dec("5"),
	}
}

// ---------------------------------------------------------------------------
// Unit: journal construction (the GL correction contract).
// ---------------------------------------------------------------------------

func TestBustJournalBalanced(t *testing.T) {
	svc := &TradeBustService{}
	tr := testBustTrade()
	j, err := svc.buildBustJournal(tr,
		BustRequest{Action: BustActionBust, Reason: "fat finger"}, 900)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := j.Validate(); err != nil {
		t.Fatalf("journal must validate: %v", err)
	}
	// Zero-sum per currency is enforced by Validate — additionally assert
	// the economics: quote leg = qty*price = 1100 USD.
	qa := tr.qty.Mul(tr.price)
	seenSellerClawback, seenFeeRev := false, 0
	for _, l := range j.Lines {
		if l.Currency == "USD" && l.Debit.Equal(qa.Sub(tr.sellerFee)) {
			seenSellerClawback = true
		}
		if strings.HasPrefix(l.AccountCode, "4010_TRADING_FEE_REVENUE") {
			seenFeeRev++
		}
	}
	if !seenSellerClawback {
		t.Fatal("seller quote clawback line missing")
	}
	if seenFeeRev != 2 {
		t.Fatalf("both fee revenue reversals required, got %d", seenFeeRev)
	}
	// Effects: buyer +1100 USD / −998 EUR; seller +1000 EUR / −1097 USD.
	want := map[string]decimal.Decimal{
		"101:USD": qa,
		"101:EUR": tr.qty.Sub(tr.buyerFee).Neg(),
		"102:EUR": tr.qty,
		"102:USD": qa.Sub(tr.sellerFee).Neg(),
	}
	got := map[string]decimal.Decimal{}
	for _, e := range j.Effects {
		got[fmt.Sprintf("%d:%s", e.AccountID, e.Currency)] = e.Net()
	}
	for k, w := range want {
		if !got[k].Equal(w) {
			t.Fatalf("effect %s = %s, want %s", k, got[k], w)
		}
	}
}

func TestBustJournalZeroFees(t *testing.T) {
	svc := &TradeBustService{}
	tr := testBustTrade()
	tr.buyerFee, tr.sellerFee = decimal.Zero, decimal.Zero
	j, err := svc.buildBustJournal(tr, BustRequest{Action: BustActionBust, Reason: "x"}, 1)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := j.Validate(); err != nil {
		t.Fatalf("zero-fee journal must still validate: %v", err)
	}
	for _, l := range j.Lines {
		if strings.HasPrefix(l.AccountCode, "4010_TRADING_FEE_REVENUE") {
			t.Fatal("zero fees must not emit revenue lines")
		}
	}
}

func TestBustJournalDeliveryIntent(t *testing.T) {
	svc := &TradeBustService{}
	tr := testBustTrade()
	tr.intent = "PHYSICAL_DELIVERY"
	j, err := svc.buildBustJournal(tr, BustRequest{Action: BustActionBust, Reason: "x"}, 1)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := j.Validate(); err != nil {
		t.Fatalf("delivery unwind must validate: %v", err)
	}
	// The pending-delivery reclass is unwound — 2011 lines present, and
	// wallet effects are pure locked→available releases (net zero).
	pd := 0
	for _, l := range j.Lines {
		if strings.HasPrefix(l.AccountCode, "2011_PENDING_SETTLEMENT_DELIVERY") {
			pd++
		}
	}
	if pd != 2 {
		t.Fatalf("want 2 pending-delivery legs, got %d", pd)
	}
	for _, e := range j.Effects {
		if !e.Net().IsZero() {
			t.Fatalf("delivery bust must not move total balance, got %+v", e)
		}
	}
}

func TestAdjustJournalDelta(t *testing.T) {
	svc := &TradeBustService{}
	tr := testBustTrade()
	// Reprice 1.10 → 1.00 on qty 1000: delta −100 USD (buyer refunded).
	j, err := svc.buildAdjustJournal(tr, BustRequest{
		Action: BustActionPriceAdjust, AdjustedPrice: dec("1.00"), Reason: "x"}, 1)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := j.Validate(); err != nil {
		t.Fatalf("adjust journal must validate: %v", err)
	}
	var tot decimal.Decimal
	for _, l := range j.Lines {
		if l.Currency != "USD" {
			t.Fatal("price adjust must be quote-currency only")
		}
		tot = tot.Add(l.Amount())
	}
	if !tot.IsZero() {
		t.Fatal("adjust journal must net to zero")
	}
	// Equal price → rejected up front.
	_, err = svc.buildAdjustJournal(tr, BustRequest{
		Action: BustActionPriceAdjust, AdjustedPrice: dec("1.10"), Reason: "x"}, 1)
	requireCode(t, err, "INVALID_REQUEST")
}

func TestBustJournalFeeExceedsProceeds(t *testing.T) {
	svc := &TradeBustService{}
	tr := testBustTrade()
	tr.sellerFee = dec("2000") // exceeds the 1100 quote leg
	_, err := svc.buildBustJournal(tr, BustRequest{Action: BustActionBust, Reason: "x"}, 1)
	requireCode(t, err, "FEE_EXCEEDS_PROCEEDS")
}

// ---------------------------------------------------------------------------
// Unit: gates that need no storage.
// ---------------------------------------------------------------------------

func TestBustServiceFailsClosedOnNilDeps(t *testing.T) {
	if _, err := NewTradeBustService(TradeBustDeps{}); err == nil {
		t.Fatal("nil deps must fail closed")
	}
	if _, err := NewInstrumentMaintenanceService(InstrumentMaintenanceDeps{}); err == nil {
		t.Fatal("nil deps must fail closed")
	}
}

func TestBustRoleGate(t *testing.T) {
	svc := &TradeBustService{roles: testRoles(map[int64]string{
		1: RoleRiskManager, 2: RoleComplianceOfficer, 3: RoleSupportAgent,
	})}
	if err := svc.requireBustRole(context.Background(), 1); err != nil {
		t.Fatalf("Risk Manager must bust: %v", err)
	}
	requireCode(t, svc.requireBustRole(context.Background(), 2), "UNAUTHORIZED_ROLE")
	requireCode(t, svc.requireBustRole(context.Background(), 3), "UNAUTHORIZED_ROLE")
	requireCode(t, svc.requireBustRole(context.Background(), 9), "INTERNAL_ERROR")
}

func TestBustApproverDistinct(t *testing.T) {
	svc := &TradeBustService{roles: testRoles(map[int64]string{
		1: RoleRiskManager, 2: RoleRiskManager, 3: RoleSupportAgent,
	})}
	requireCode(t, svc.requireApprover(context.Background(), 1, 1), "DUAL_CONTROL_REQUIRED")
	requireCode(t, svc.requireApprover(context.Background(), 1, 0), "DUAL_CONTROL_REQUIRED")
	requireCode(t, svc.requireApprover(context.Background(), 1, 3), "UNAUTHORIZED_ROLE")
	if err := svc.requireApprover(context.Background(), 1, 2); err != nil {
		t.Fatalf("distinct Risk Manager approver must pass: %v", err)
	}
}

func TestBustRequestValidation(t *testing.T) {
	svc := &TradeBustService{}
	requireCode(t, svc.validateRequest(BustRequest{TradeID: 0}), "INVALID_REQUEST")
	requireCode(t, svc.validateRequest(BustRequest{TradeID: 1, Action: "X", Reason: "r"}), "INVALID_REQUEST")
	requireCode(t, svc.validateRequest(BustRequest{TradeID: 1, Action: BustActionBust}), "INVALID_REQUEST") // no reason
	requireCode(t, svc.validateRequest(BustRequest{
		TradeID: 1, Action: BustActionPriceAdjust, Reason: "r"}), "INVALID_REQUEST") // no adjusted price
	if err := svc.validateRequest(BustRequest{
		TradeID: 1, Action: BustActionBust, Reason: "fat finger"}); err != nil {
		t.Fatalf("valid bust request rejected: %v", err)
	}
}

func TestSensitiveOpRegistered(t *testing.T) {
	if !SensitiveOperation(OpTradeBust) {
		t.Fatal("trade-bust must be a §8.2 sensitive operation")
	}
}

// ---------------------------------------------------------------------------
// Unit: instrument maintenance validation + session boundary.
// ---------------------------------------------------------------------------

func TestPairSymbolValidation(t *testing.T) {
	valid := []string{"EUR/USD", "USD/JPY", "gbp/usd"} // normalized to upper
	for _, s := range valid {
		b, q, err := ValidatePairSymbol(s)
		if err != nil || len(b) != 3 || len(q) != 3 {
			t.Fatalf("symbol %q must validate: %v", s, err)
		}
	}
	bad := []string{"EURUSD", "EUR/UD", "EU/USD", "EUR/USD/JPY", "eur/us",
		"BTC/USD", "EUR/", "/USD", ""}
	for _, s := range bad {
		if _, _, err := ValidatePairSymbol(s); err == nil {
			t.Fatalf("symbol %q must reject", s)
		}
	}
	if _, _, err := ValidatePairSymbol("USD/USD"); err == nil {
		t.Fatal("base == quote must reject")
	}
	// Space-trimmed valid symbol still validates.
	if _, _, err := ValidatePairSymbol(" EUR/USD "); err != nil {
		t.Fatalf("trimmed symbol must validate: %v", err)
	}
}

func TestParamValidation(t *testing.T) {
	ok := map[string]string{
		"tick_size": "0.0001", "lot_size": "1000", "max_leverage": "30",
		"settlement_cycle": "1", "margin_rate": "0.0333",
		"trading_hours": `{"open":"21:00Z","close":"22:00Z"}`,
	}
	for f, v := range ok {
		if err := validateParam(f, v); err != nil {
			t.Fatalf("param %s=%q must validate: %v", f, v, err)
		}
	}
	bad := map[string]string{
		"not_a_field": "1", "tick_size": "-1", "tick_size2": "1",
		"max_leverage": "0", "max_leverage2": "30", "settlement_cycle": "9",
		"margin_rate": "abc", "trading_hours": "not-json",
	}
	for f, v := range bad {
		if err := validateParam(f, v); err == nil {
			t.Fatalf("param %s=%q must reject", f, v)
		}
	}
	// tick_size2 is whitelisted? no — only exact field names.
	if err := validateParam("status", "HALTED"); err == nil {
		t.Fatal("lifecycle status is not a maintenance parameter")
	}
}

func TestDefaultNextSessionStart(t *testing.T) {
	// Wednesday 10:00 → same-day 22:00 UTC.
	got := DefaultNextSessionStart(time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC))
	if got.Hour() != 22 || got.Day() != 16 {
		t.Fatalf("mid-week morning → same day 22:00, got %s", got)
	}
	// Friday 23:00 → skip Saturday → Sunday 22:00.
	got = DefaultNextSessionStart(time.Date(2026, 9, 18, 23, 0, 0, 0, time.UTC))
	if got.Weekday() == time.Saturday {
		t.Fatalf("venue closed Saturday — boundary must skip it: %s", got)
	}
	if got.Weekday() != time.Sunday {
		t.Fatalf("next boundary after Friday close should be Sunday, got %s", got)
	}
}

func TestMaintenanceRoleGate(t *testing.T) {
	svc := &InstrumentMaintenanceService{roles: testRoles(map[int64]string{
		1: RoleRiskManager, 2: RoleComplianceOfficer, 3: RoleSuperAdmin,
		4: RoleSupportAgent,
	})}
	if err := svc.requireRole(context.Background(), 1, maintProposeRoles, "x"); err != nil {
		t.Fatalf("Risk Manager must propose: %v", err)
	}
	requireCode(t, svc.requireRole(context.Background(), 2, maintProposeRoles, "x"), "UNAUTHORIZED_ROLE")
	if err := svc.requireRole(context.Background(), 2, maintReviewRoles, "x"); err != nil {
		t.Fatalf("Compliance must review: %v", err)
	}
	requireCode(t, svc.requireRole(context.Background(), 1, maintReviewRoles, "x"), "UNAUTHORIZED_ROLE")
	requireCode(t, svc.requireRole(context.Background(), 4, maintParamRoles, "x"), "UNAUTHORIZED_ROLE")
	if err := svc.requireRole(context.Background(), 3, maintApproveRoles, "x"); err != nil {
		t.Fatalf("Super Admin must approve: %v", err)
	}
}

// ---------------------------------------------------------------------------
// PG-gated integration tests.
// ---------------------------------------------------------------------------

const bustTestDSN = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"

// bustFixtureDDL mirrors the pre-existing tables migrations 051/219/090
// need (documented contract — the partitioned trades table cannot be
// created without pg_partman, so the fixture is a plain table with the
// same columns and the same (id, created_at) PK shape).
const bustFixtureDDL = `
CREATE TABLE users (
    id BIGSERIAL PRIMARY KEY
);
CREATE TABLE accounts (
    id      BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users (id)
);
CREATE TYPE instrument_type_enum   AS ENUM ('SPOT','FORWARD','SWAP','NDF','OPTION');
CREATE TYPE instrument_status_enum AS ENUM ('DRAFT','ACTIVE','CANCEL_ONLY','SUSPENDED','HALTED','RESTRICTED','DELISTED');
CREATE TABLE instruments (
    id                  BIGSERIAL PRIMARY KEY,
    symbol              VARCHAR(32)  NOT NULL UNIQUE,
    base_currency       VARCHAR(3)   NOT NULL,
    quote_currency      VARCHAR(3)   NOT NULL,
    instrument_type     instrument_type_enum NOT NULL,
    tick_size           DECIMAL(20,8) NOT NULL,
    lot_size            DECIMAL(20,8) NOT NULL,
    min_order_qty       DECIMAL(20,8) NOT NULL,
    max_order_qty       DECIMAL(20,8) NOT NULL,
    min_notional        DECIMAL(28,8) NOT NULL DEFAULT 0,
    min_price           DECIMAL(20,8),
    max_price           DECIMAL(20,8),
    price_band_pct_up   DECIMAL(5,2)  NOT NULL DEFAULT 2.00,
    price_band_pct_down DECIMAL(5,2)  NOT NULL DEFAULT 5.00,
    max_spread_pips     DECIMAL(12,4),
    max_open_orders     INTEGER,
    max_algo_orders     INTEGER,
    settlement_cycle    SMALLINT      NOT NULL,
    max_leverage        INTEGER       NOT NULL,
    status              instrument_status_enum NOT NULL DEFAULT 'DRAFT',
    created_at          TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ   NOT NULL DEFAULT now()
);
CREATE TABLE orders (
    id                BIGSERIAL PRIMARY KEY,
    account_id        BIGINT NOT NULL REFERENCES accounts (id),
    instrument_id     BIGINT NOT NULL REFERENCES instruments (id),
    settlement_intent VARCHAR(20)          -- NULL = inherit (fixture)
);
CREATE TABLE trades (
    id                BIGINT GENERATED ALWAYS AS IDENTITY,
    instrument_id     BIGINT NOT NULL,
    buy_order_id      BIGINT NOT NULL,
    sell_order_id     BIGINT NOT NULL,
    buyer_account_id  BIGINT NOT NULL,
    seller_account_id BIGINT NOT NULL,
    price             DECIMAL(20,8) NOT NULL,
    quantity          DECIMAL(28,8) NOT NULL,
    buyer_fee         DECIMAL(20,8),
    seller_fee        DECIMAL(20,8),
    settlement_date   DATE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
);
CREATE TYPE settlement_direction_enum AS ENUM ('PAY','RECEIVE');
CREATE TYPE settlement_status_enum    AS ENUM ('PENDING','SETTLED','FAILED','RECONCILED');
CREATE TABLE settlement_instructions (
    id            BIGSERIAL PRIMARY KEY,
    trade_id      BIGINT NOT NULL,
    account_id    BIGINT NOT NULL,
    currency      VARCHAR(3) NOT NULL,
    amount        DECIMAL(28,8) NOT NULL,
    direction     settlement_direction_enum NOT NULL,
    status        settlement_status_enum NOT NULL DEFAULT 'PENDING',
    dispatched_at TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE positions (
    id             BIGSERIAL PRIMARY KEY,
    account_id     BIGINT NOT NULL,
    instrument_id  BIGINT NOT NULL,
    side           VARCHAR(5) NOT NULL,
    quantity       DECIMAL(28,8) NOT NULL,
    entry_price    DECIMAL(20,8) NOT NULL,
    mark_price     DECIMAL(20,8),
    unrealized_pnl DECIMAL(28,8) NOT NULL DEFAULT 0,
    realized_pnl   DECIMAL(28,8) NOT NULL DEFAULT 0,
    margin_used    DECIMAL(28,8) NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (account_id, instrument_id)
);
CREATE TABLE position_fills (
    trade_id      BIGINT        NOT NULL,
    account_id    BIGINT        NOT NULL,
    instrument_id BIGINT        NOT NULL,
    side          VARCHAR(4)    NOT NULL CHECK (side IN ('BUY','SELL')),
    quantity      DECIMAL(28,8) NOT NULL,
    price         DECIMAL(20,8) NOT NULL,
    realized_pnl  DECIMAL(28,8) NOT NULL DEFAULT 0,
    applied_at    TIMESTAMPTZ   NOT NULL DEFAULT now(),
    PRIMARY KEY (trade_id, account_id)
);
CREATE TABLE admin_audit_log (
    id            BIGSERIAL PRIMARY KEY,
    admin_user_id BIGINT NOT NULL,
    action        VARCHAR(128) NOT NULL,
    target_type   VARCHAR(64),
    target_id     BIGINT,
    before_state  JSONB,
    after_state   JSONB,
    ip_address    INET,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE audit_hash_chain (
    id           BIGSERIAL PRIMARY KEY,
    sequence_num BIGINT NOT NULL UNIQUE,
    table_name   VARCHAR(64) NOT NULL,
    record_id    BIGINT,
    action       VARCHAR(16) NOT NULL,
    payload_hash VARCHAR(64) NOT NULL,
    prev_hash    VARCHAR(64),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);`

func bustMigExec(t *testing.T, ctx context.Context, dsn, schema, file string) {
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

type bustPG struct {
	pool   *pgxpool.Pool
	schema string
	dsn    string
}

// bustPGFixture builds the scratch schema: fixture tables + the REAL
// migrations 051 (trade_busts), 219 (instrument change log) and 090
// (admin RBAC + dual-control queue).
func bustPGFixture(t *testing.T) *bustPG {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres integration tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = bustTestDSN
	}
	schema := fmt.Sprintf("bust_itest_%d", time.Now().UnixNano())
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

	// Fixture tables inside the scratch schema.
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
	if _, err := conn.Exec(ctx, bustFixtureDDL); err != nil {
		conn.Close(ctx)
		t.Fatalf("fixture DDL: %v", err)
	}
	conn.Close(ctx)

	migDir := "../db/migrations"
	bustMigExec(t, ctx, dsn, schema, migDir+"/051_trade_busts.up.sql")
	bustMigExec(t, ctx, dsn, schema, migDir+"/219_instrument_change_log.up.sql")
	bustMigExec(t, ctx, dsn, schema, migDir+"/090_admin_rbac.up.sql")

	pool, err := pgxpool.New(ctx,
		dsn+"&search_path="+schema)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("pool ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return &bustPG{pool: pool, schema: schema, dsn: dsn}
}

// seedAdmins inserts users + ACTIVE venue bindings (migration 090 caps:
// ≤12mo general, ≤90d for Super Admin, ≤4h BREAK_GLASS — 30d STANDARD
// bindings satisfy all three).
func (pg *bustPG) seedAdmins(t *testing.T, grants map[int64]string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pg.pool.Exec(ctx,
		`INSERT INTO users (id) VALUES (1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("granter user: %v", err)
	}
	for uid, role := range grants {
		if _, err := pg.pool.Exec(ctx,
			`INSERT INTO users (id) VALUES ($1) ON CONFLICT DO NOTHING`, uid); err != nil {
			t.Fatalf("user %d: %v", uid, err)
		}
		if _, err := pg.pool.Exec(ctx, `
			INSERT INTO admin_role_bindings (user_id, role, kind, granter_id, expires_at)
			VALUES ($1,$2,'STANDARD',1, now() + interval '30 days')`,
			uid, role); err != nil {
			t.Fatalf("binding %d/%s: %v", uid, role, err)
		}
	}
}

// seedTrade inserts instrument/accounts/orders/trade (+ optional
// settlement legs + position fills) and returns ids.
func (pg *bustPG) seedTrade(t *testing.T, price, refPrice, qty, buyFee, sellFee string,
	createdAt time.Time, settled bool) (tradeID, instID, buyerAcct, sellerAcct int64) {
	t.Helper()
	ctx := context.Background()

	if _, err := pg.pool.Exec(ctx,
		`INSERT INTO users (id) VALUES (901),(902) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("trade users: %v", err)
	}
	err := pg.pool.QueryRow(ctx, `
		INSERT INTO instruments
		    (symbol, base_currency, quote_currency, instrument_type,
		     tick_size, lot_size, min_order_qty, max_order_qty,
		     price_band_pct_up, price_band_pct_down, settlement_cycle,
		     max_leverage, status)
		VALUES ('EUR/USD','EUR','USD','SPOT',0.0001,1000,100,1000000,
		        5.00,5.00,1,30,'ACTIVE')
		RETURNING id`).Scan(&instID)
	if err != nil {
		t.Fatalf("instrument: %v", err)
	}
	for _, u := range []int64{901, 902} {
		if _, err := pg.pool.Exec(ctx,
			`INSERT INTO accounts (user_id) VALUES ($1)`, u); err != nil {
			t.Fatalf("account %d: %v", u, err)
		}
	}
	if err := pg.pool.QueryRow(ctx,
		`SELECT id FROM accounts WHERE user_id=901 ORDER BY id LIMIT 1`).Scan(&buyerAcct); err != nil {
		t.Fatalf("buyer acct: %v", err)
	}
	if err := pg.pool.QueryRow(ctx,
		`SELECT id FROM accounts WHERE user_id=902 ORDER BY id LIMIT 1`).Scan(&sellerAcct); err != nil {
		t.Fatalf("seller acct: %v", err)
	}
	var buyOrder, sellOrder int64
	if err := pg.pool.QueryRow(ctx,
		`INSERT INTO orders (account_id, instrument_id, settlement_intent)
		 VALUES ($1,$2,'ROLLING_MARGIN') RETURNING id`, buyerAcct, instID).Scan(&buyOrder); err != nil {
		t.Fatalf("buy order: %v", err)
	}
	if err := pg.pool.QueryRow(ctx,
		`INSERT INTO orders (account_id, instrument_id, settlement_intent)
		 VALUES ($1,$2,'ROLLING_MARGIN') RETURNING id`, sellerAcct, instID).Scan(&sellOrder); err != nil {
		t.Fatalf("sell order: %v", err)
	}
	// Reference trade one minute before the fill anchors the market price.
	if _, err := pg.pool.Exec(ctx, `
		INSERT INTO trades (instrument_id, buy_order_id, sell_order_id,
		    buyer_account_id, seller_account_id, price, quantity, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,10,$7)`,
		instID, buyOrder, sellOrder, buyerAcct, sellerAcct, refPrice,
		createdAt.Add(-time.Minute)); err != nil {
		t.Fatalf("ref trade: %v", err)
	}
	err = pg.pool.QueryRow(ctx, `
		INSERT INTO trades (instrument_id, buy_order_id, sell_order_id,
		    buyer_account_id, seller_account_id, price, quantity,
		    buyer_fee, seller_fee, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		instID, buyOrder, sellOrder, buyerAcct, sellerAcct, price, qty,
		buyFee, sellFee, createdAt).Scan(&tradeID)
	if err != nil {
		t.Fatalf("trade: %v", err)
	}
	// Settlement legs: quote leg = qty×price, base leg = qty.
	status := "PENDING"
	var dispatched any
	if settled {
		status = "SETTLED"
	}
	qa := decimal.RequireFromString(qty).Mul(decimal.RequireFromString(price))
	if _, err := pg.pool.Exec(ctx, `
		INSERT INTO settlement_instructions
		    (trade_id, account_id, currency, amount, direction, status, dispatched_at)
		VALUES
		    ($1,$2,'USD',$3,'PAY',$5,$6),
		    ($1,$4,'EUR',$7,'PAY',$5,$6),
		    ($1,$2,'EUR',$7,'RECEIVE',$5,$6),
		    ($1,$4,'USD',$3,'RECEIVE',$5,$6)`,
		tradeID, buyerAcct, qa.String(), sellerAcct, status, dispatched, qty); err != nil {
		t.Fatalf("settlement legs: %v", err)
	}
	if settled {
		if _, err := pg.pool.Exec(ctx,
			`UPDATE settlement_instructions SET dispatched_at=now()
			  WHERE trade_id=$1`, tradeID); err != nil {
			t.Fatalf("dispatch legs: %v", err)
		}
	}
	// Position fills — the projection rows.
	for _, leg := range []struct {
		acct int64
		side string
	}{{buyerAcct, "BUY"}, {sellerAcct, "SELL"}} {
		if _, err := pg.pool.Exec(ctx, `
			INSERT INTO position_fills (trade_id, account_id, instrument_id, side, quantity, price)
			VALUES ($1,$2,$3,$4,$5,$6)`, tradeID, leg.acct, instID, leg.side, qty, price); err != nil {
			t.Fatalf("position fill: %v", err)
		}
	}
	// Materialized positions (what a live venue would hold).
	if _, err := pg.pool.Exec(ctx, `
		INSERT INTO positions (account_id, instrument_id, side, quantity, entry_price)
		VALUES ($1,$3,'LONG',$2,$4), ($5,$3,'SHORT',$2,$4)
		ON CONFLICT (account_id, instrument_id) DO NOTHING`,
		buyerAcct, qty, instID, price, sellerAcct); err != nil {
		t.Fatalf("positions: %v", err)
	}
	return tradeID, instID, buyerAcct, sellerAcct
}

func newBustService(t *testing.T, pg *bustPG, roles map[int64]string,
	now time.Time) (*TradeBustService, *bustFakePoster, *bustFakePub, *bustFakeNotifier) {
	poster := &bustFakePoster{}
	pub := &bustFakePub{}
	notifier := &bustFakeNotifier{}
	svc, err := NewTradeBustService(TradeBustDeps{
		Pool: pg.pool, Poster: poster, Locks: &bustFakeLocker{},
		Publisher: pub, Roles: testRoles(roles), Notifier: notifier,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return svc, poster, pub, notifier
}

func TestBustExecutePG(t *testing.T) {
	pg := bustPGFixture(t)
	pg.seedAdmins(t, map[int64]string{1001: RoleRiskManager, 1002: RoleRiskManager})
	now := time.Now()
	tradeID, _, buyer, seller := pg.seedTrade(t, "1.15", "1.00", "1000", "2", "3",
		now.Add(-2*time.Minute), false)

	svc, poster, pub, notifier := newBustService(t, pg,
		map[int64]string{1001: RoleRiskManager, 1002: RoleRiskManager}, now)

	// Synchronous two-principal completion.
	out, err := svc.RequestBust(context.Background(),
		AdminActor{UserID: 1001, ClientIP: "127.0.0.1"}, BustRequest{
			TradeID: tradeID, Action: BustActionBust,
			Reason: "obvious error — price 10% off market", ApproverID: 1002,
		})
	if err != nil {
		t.Fatalf("bust: %v", err)
	}
	if out.Bust.Status != BustStatusExecuted {
		t.Fatalf("status %s want EXECUTED", out.Bust.Status)
	}
	if out.Settlement != "VOIDED" {
		t.Fatalf("settlement outcome %s want VOIDED", out.Settlement)
	}
	if !out.Dispatched {
		t.Fatal("balance events must dispatch post-commit")
	}
	if len(poster.journals) != 1 {
		t.Fatalf("one reversal journal, got %d", len(poster.journals))
	}
	if len(pub.subjects) == 0 {
		t.Fatal("BalanceChanged subjects dispatched")
	}

	var status string
	if err := pg.pool.QueryRow(context.Background(),
		`SELECT status::text FROM trades WHERE id=$1`, tradeID).Scan(&status); err != nil {
		t.Fatalf("trade status: %v", err)
	}
	if status != "BUSTED" {
		t.Fatalf("trade status %s want BUSTED", status)
	}
	var settled int
	if err := pg.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM settlement_instructions
		  WHERE trade_id=$1 AND status='PENDING'`, tradeID).Scan(&settled); err != nil {
		t.Fatalf("settlement probe: %v", err)
	}
	if settled != 0 {
		t.Fatalf("%d legs left PENDING after bust", settled)
	}
	var void int
	if err := pg.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM settlement_instructions
		  WHERE trade_id=$1 AND status='VOID'`, tradeID).Scan(&void); err != nil {
		t.Fatalf("void probe: %v", err)
	}
	if void != 4 {
		t.Fatalf("want 4 VOID legs, got %d", void)
	}
	// Positions rebuilt from fills — the busted trade is excluded → flat.
	var qty decimal.Decimal
	var side string
	if err := pg.pool.QueryRow(context.Background(),
		`SELECT quantity, side::text FROM positions WHERE account_id=$1`,
		buyer).Scan(&qty, &side); err != nil {
		t.Fatalf("buyer position: %v", err)
	}
	if !qty.IsZero() {
		t.Fatalf("buyer position must be flat after bust, got %s %s", qty, side)
	}
	if err := pg.pool.QueryRow(context.Background(),
		`SELECT quantity FROM positions WHERE account_id=$1`, seller).Scan(&qty); err != nil {
		t.Fatalf("seller position: %v", err)
	}
	if !qty.IsZero() {
		t.Fatalf("seller position must be flat after bust, got %s", qty)
	}
	// Both counterparties notified.
	if len(notifier.events) != 2 ||
		notifier.events[0] != BustEventBusted || notifier.events[1] != BustEventBusted {
		t.Fatalf("counterparty notices: %v", notifier.events)
	}
	// Audit trail exists.
	var auditCount int
	if err := pg.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM admin_audit_log WHERE action='trade.bust.execute'`).Scan(&auditCount); err != nil {
		t.Fatalf("audit probe: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("want 1 bust audit row, got %d", auditCount)
	}
}

func TestBustOfBustedPG(t *testing.T) {
	pg := bustPGFixture(t)
	pg.seedAdmins(t, map[int64]string{1001: RoleRiskManager, 1002: RoleRiskManager})
	now := time.Now()
	tradeID, _, _, _ := pg.seedTrade(t, "1.15", "1.00", "1000", "2", "3",
		now.Add(-2*time.Minute), false)
	svc, _, _, _ := newBustService(t, pg,
		map[int64]string{1001: RoleRiskManager, 1002: RoleRiskManager}, now)

	_, err := svc.RequestBust(context.Background(), AdminActor{UserID: 1001},
		BustRequest{TradeID: tradeID, Action: BustActionBust,
			Reason: "first", ApproverID: 1002})
	if err != nil {
		t.Fatalf("first bust: %v", err)
	}
	_, err = svc.RequestBust(context.Background(), AdminActor{UserID: 1002},
		BustRequest{TradeID: tradeID, Action: BustActionBust,
			Reason: "second", ApproverID: 1001})
	requireCode(t, err, "INVALID_REQUEST") // busted is terminal — flagged, never re-busted
}

func TestBustWindowExpiredPG(t *testing.T) {
	pg := bustPGFixture(t)
	pg.seedAdmins(t, map[int64]string{1001: RoleRiskManager})
	now := time.Now()
	tradeID, _, _, _ := pg.seedTrade(t, "1.15", "1.00", "1000", "2", "3",
		now.Add(-20*time.Minute), false)
	svc, _, _, _ := newBustService(t, pg,
		map[int64]string{1001: RoleRiskManager}, now)

	_, err := svc.RequestBust(context.Background(), AdminActor{UserID: 1001},
		BustRequest{TradeID: tradeID, Action: BustActionBust,
			Reason: "late", ApproverID: 1001})
	// Approver==maker trips first (distinctness is checked before execution)
	// — use a pending request to isolate the window gate.
	requireCode(t, err, "DUAL_CONTROL_REQUIRED")

	out, err := svc.RequestBust(context.Background(), AdminActor{UserID: 1001},
		BustRequest{TradeID: tradeID, Action: BustActionBust, Reason: "late"})
	requireCode(t, err, "TRADE_ALREADY_SETTLED") // §7.3.4 15-minute deadline
	if out != nil {
		t.Fatal("expired window must not create a review row")
	}
}

func TestBustSettledRejectsPG(t *testing.T) {
	pg := bustPGFixture(t)
	pg.seedAdmins(t, map[int64]string{1001: RoleRiskManager})
	now := time.Now()
	tradeID, _, _, _ := pg.seedTrade(t, "1.15", "1.00", "1000", "2", "3",
		now.Add(-2*time.Minute), true) // dispatched/settled legs
	svc, _, _, _ := newBustService(t, pg,
		map[int64]string{1001: RoleRiskManager}, now)

	_, err := svc.RequestBust(context.Background(), AdminActor{UserID: 1001},
		BustRequest{TradeID: tradeID, Action: BustActionBust, Reason: "too late"})
	requireCode(t, err, "TRADE_ALREADY_SETTLED")
}

func TestBustPendingApprovePG(t *testing.T) {
	pg := bustPGFixture(t)
	pg.seedAdmins(t, map[int64]string{
		1001: RoleRiskManager, 1002: RoleRiskManager, 1003: RoleSupportAgent,
	})
	now := time.Now()
	tradeID, _, _, _ := pg.seedTrade(t, "1.15", "1.00", "1000", "2", "3",
		now.Add(-2*time.Minute), false)
	svc, poster, _, _ := newBustService(t, pg,
		map[int64]string{1001: RoleRiskManager, 1002: RoleRiskManager,
			1003: RoleSupportAgent}, now)

	out, err := svc.RequestBust(context.Background(), AdminActor{UserID: 1001},
		BustRequest{TradeID: tradeID, Action: BustActionBust, Reason: "flag"})
	if err != nil {
		t.Fatalf("pending request: %v", err)
	}
	if out.Bust.Status != BustStatusPending || out.Settlement != "PENDING" {
		t.Fatalf("want PENDING review, got %+v", out.Bust)
	}
	// The settlement hold seam reports the review.
	held, err := svc.HasPendingBust(context.Background(), tradeID)
	if err != nil || !held {
		t.Fatalf("HasPendingBust must report the open review: %v %v", held, err)
	}
	// Second request while under review → TRADE_BUST_PENDING.
	_, err = svc.RequestBust(context.Background(), AdminActor{UserID: 1002},
		BustRequest{TradeID: tradeID, Action: BustActionBust, Reason: "again"})
	requireCode(t, err, "TRADE_BUST_PENDING")
	// Self-approval → DUAL_CONTROL_VIOLATION.
	_, err = svc.ApproveBust(context.Background(), AdminActor{UserID: 1001}, out.Bust.ID)
	requireCode(t, err, "DUAL_CONTROL_VIOLATION")
	// Ineligible approver.
	_, err = svc.ApproveBust(context.Background(), AdminActor{UserID: 1003}, out.Bust.ID)
	requireCode(t, err, "UNAUTHORIZED_ROLE")
	// Distinct Risk Manager approves → executed.
	fin, err := svc.ApproveBust(context.Background(), AdminActor{UserID: 1002}, out.Bust.ID)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if fin.Bust.Status != BustStatusExecuted || len(poster.journals) != 1 {
		t.Fatalf("execution did not land: %+v", fin)
	}
	var st string
	_ = pg.pool.QueryRow(context.Background(),
		`SELECT status::text FROM trades WHERE id=$1`, tradeID).Scan(&st)
	if st != "BUSTED" {
		t.Fatalf("trade status %s want BUSTED", st)
	}
	// Approver recorded on the §5.29 row.
	if fin.Bust.ApprovedBy == nil || *fin.Bust.ApprovedBy != 1002 {
		t.Fatalf("approved_by %v want 1002", fin.Bust.ApprovedBy)
	}
}

func TestPartialFillBustPG(t *testing.T) {
	pg := bustPGFixture(t)
	pg.seedAdmins(t, map[int64]string{1001: RoleRiskManager, 1002: RoleRiskManager})
	now := time.Now()
	// Two fills on the same order set — busting trade A must leave trade
	// B's position intact.
	tradeA, inst, buyer, seller := pg.seedTrade(t, "1.15", "1.00", "600", "1", "1",
		now.Add(-4*time.Minute), false)
	// Second partial fill on the same orders (same ids reused).
	var buyOrder, sellOrder int64
	_ = pg.pool.QueryRow(context.Background(),
		`SELECT buy_order_id, sell_order_id FROM trades WHERE id=$1`,
		tradeA).Scan(&buyOrder, &sellOrder)
	var tradeB int64
	err := pg.pool.QueryRow(context.Background(), `
		INSERT INTO trades (instrument_id, buy_order_id, sell_order_id,
		    buyer_account_id, seller_account_id, price, quantity, created_at)
		VALUES ($1,$2,$3,$4,$5,1.10,400,$6) RETURNING id`,
		inst, buyOrder, sellOrder, buyer, seller, now.Add(-3*time.Minute)).Scan(&tradeB)
	if err != nil {
		t.Fatalf("trade B: %v", err)
	}
	for _, leg := range []struct {
		acct int64
		side string
	}{{buyer, "BUY"}, {seller, "SELL"}} {
		if _, err := pg.pool.Exec(context.Background(), `
			INSERT INTO position_fills (trade_id, account_id, instrument_id, side, quantity, price)
			VALUES ($1,$2,$3,$4,400,1.10)`,
			tradeB, leg.acct, inst, leg.side); err != nil {
			t.Fatalf("fill B: %v", err)
		}
	}
	if _, err := pg.pool.Exec(context.Background(), `
		UPDATE positions SET quantity=1000, updated_at=now()
		 WHERE instrument_id=$1`, inst); err != nil {
		t.Fatalf("position bump: %v", err)
	}

	svc, _, _, _ := newBustService(t, pg,
		map[int64]string{1001: RoleRiskManager, 1002: RoleRiskManager}, now)
	if _, err := svc.RequestBust(context.Background(), AdminActor{UserID: 1001},
		BustRequest{TradeID: tradeA, Action: BustActionBust,
			Reason: "bust partial fill A", ApproverID: 1002}); err != nil {
		t.Fatalf("bust A: %v", err)
	}
	var qty decimal.Decimal
	if err := pg.pool.QueryRow(context.Background(),
		`SELECT quantity FROM positions WHERE account_id=$1 AND instrument_id=$2`,
		buyer, inst).Scan(&qty); err != nil {
		t.Fatalf("buyer position: %v", err)
	}
	if !qty.Equal(dec("400")) {
		t.Fatalf("partial bust must leave fill B's 400, got %s", qty)
	}
}

func TestBustAfterHedgePG(t *testing.T) {
	// Downstream hedge: the buyer closed the position with a later fill.
	// The bust must rebuild the aggregate correctly — the hedge stays,
	// so the replayed position nets to the hedge's remainder.
	pg := bustPGFixture(t)
	pg.seedAdmins(t, map[int64]string{1001: RoleRiskManager, 1002: RoleRiskManager})
	now := time.Now()
	tradeA, inst, buyer, seller := pg.seedTrade(t, "1.15", "1.00", "1000", "2", "3",
		now.Add(-4*time.Minute), false)
	var buyOrder, sellOrder int64
	_ = pg.pool.QueryRow(context.Background(),
		`SELECT buy_order_id, sell_order_id FROM trades WHERE id=$1`,
		tradeA).Scan(&buyOrder, &sellOrder)
	// Buyer hedges flat with a later SELL fill (different counterparty
	// account — reuse seller for simplicity of fixture).
	var hedge int64
	err := pg.pool.QueryRow(context.Background(), `
		INSERT INTO trades (instrument_id, buy_order_id, sell_order_id,
		    buyer_account_id, seller_account_id, price, quantity, created_at)
		VALUES ($1,$2,$3,$4,$5,1.11,1000,$6) RETURNING id`,
		inst, buyOrder, sellOrder, seller, buyer, now.Add(-3*time.Minute)).Scan(&hedge)
	if err != nil {
		t.Fatalf("hedge trade: %v", err)
	}
	// Buyer SOLD the instrument to seller (hedge out).
	for _, leg := range []struct {
		acct int64
		side string
	}{{seller, "BUY"}, {buyer, "SELL"}} {
		if _, err := pg.pool.Exec(context.Background(), `
			INSERT INTO position_fills (trade_id, account_id, instrument_id, side, quantity, price)
			VALUES ($1,$2,$3,$4,1000,1.11)`, hedge, leg.acct, inst, leg.side); err != nil {
			t.Fatalf("hedge fill: %v", err)
		}
	}
	if _, err := pg.pool.Exec(context.Background(), `
		UPDATE positions SET quantity=0, updated_at=now()
		 WHERE instrument_id=$1`, inst); err != nil {
		t.Fatalf("flat hedge: %v", err)
	}

	svc, _, _, _ := newBustService(t, pg,
		map[int64]string{1001: RoleRiskManager, 1002: RoleRiskManager}, now)
	if _, err := svc.RequestBust(context.Background(), AdminActor{UserID: 1001},
		BustRequest{TradeID: tradeA, Action: BustActionBust,
			Reason: "bust post-hedge", ApproverID: 1002}); err != nil {
		t.Fatalf("hedged bust: %v", err)
	}
	// Buyer: +1000@1.10 (busted→skipped) then −1000@1.11 → SHORT 1000.
	var qty decimal.Decimal
	var side string
	if err := pg.pool.QueryRow(context.Background(),
		`SELECT quantity, side::text FROM positions WHERE account_id=$1 AND instrument_id=$2`,
		buyer, inst).Scan(&qty, &side); err != nil {
		t.Fatalf("buyer position: %v", err)
	}
	if !qty.Equal(dec("1000")) || side != "SHORT" {
		t.Fatalf("post-hedge bust must leave buyer SHORT 1000, got %s %s", qty, side)
	}
}

func TestPriceAdjustPG(t *testing.T) {
	pg := bustPGFixture(t)
	pg.seedAdmins(t, map[int64]string{1001: RoleRiskManager, 1002: RoleRiskManager})
	now := time.Now()
	tradeID, inst, buyer, _ := pg.seedTrade(t, "1.15", "1.00", "1000", "2", "3",
		now.Add(-2*time.Minute), false)
	svc, poster, _, notifier := newBustService(t, pg,
		map[int64]string{1001: RoleRiskManager, 1002: RoleRiskManager}, now)

	out, err := svc.RequestBust(context.Background(), AdminActor{UserID: 1001},
		BustRequest{TradeID: tradeID, Action: BustActionPriceAdjust,
			AdjustedPrice: dec("1.00"), Reason: "reprice to market",
			ApproverID: 1002})
	if err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if out.Settlement != "AMENDED" {
		t.Fatalf("settlement %s want AMENDED", out.Settlement)
	}
	var status string
	_ = pg.pool.QueryRow(context.Background(),
		`SELECT status::text FROM trades WHERE id=$1`, tradeID).Scan(&status)
	if status != "PRICE_ADJUSTED" {
		t.Fatalf("trade status %s want PRICE_ADJUSTED", status)
	}
	// Original trade price retained (flagged, never rewritten).
	var price decimal.Decimal
	_ = pg.pool.QueryRow(context.Background(),
		`SELECT price FROM trades WHERE id=$1`, tradeID).Scan(&price)
	if !price.Equal(dec("1.15")) {
		t.Fatalf("original price must be retained, got %s", price)
	}
	// USD settlement legs amended 1150 → 1000.
	var amt decimal.Decimal
	_ = pg.pool.QueryRow(context.Background(),
		`SELECT amount FROM settlement_instructions
		  WHERE trade_id=$1 AND currency='USD' AND direction='PAY'`,
		tradeID).Scan(&amt)
	if !amt.Equal(dec("1000")) {
		t.Fatalf("USD leg must amend to 1000, got %s", amt)
	}
	// Position replay repriced the fill → unrealized mark-off moves.
	var entry decimal.Decimal
	_ = pg.pool.QueryRow(context.Background(),
		`SELECT entry_price FROM positions WHERE account_id=$1 AND instrument_id=$2`,
		buyer, inst).Scan(&entry)
	if !entry.Equal(dec("1.00")) {
		t.Fatalf("position entry repriced to 1.00, got %s", entry)
	}
	if len(poster.journals) != 1 {
		t.Fatalf("one delta journal, got %d", len(poster.journals))
	}
	if notifier.events[0] != BustEventPriceAdjusted {
		t.Fatalf("event %s want %s", notifier.events[0], BustEventPriceAdjusted)
	}
}

func TestBustInsideBandRejectsPG(t *testing.T) {
	pg := bustPGFixture(t)
	pg.seedAdmins(t, map[int64]string{1001: RoleRiskManager})
	now := time.Now()
	// Fill at 1.04 vs ref 1.00 → 4% < 2×5% band → not an obvious error.
	tradeID, _, _, _ := pg.seedTrade(t, "1.04", "1.00", "1000", "2", "3",
		now.Add(-2*time.Minute), false)
	svc, _, _, _ := newBustService(t, pg,
		map[int64]string{1001: RoleRiskManager}, now)
	_, err := svc.RequestBust(context.Background(), AdminActor{UserID: 1001},
		BustRequest{TradeID: tradeID, Action: BustActionBust, Reason: "not an error"})
	requireCode(t, err, "INVALID_REQUEST")
}

func TestBustExpirePendingPG(t *testing.T) {
	pg := bustPGFixture(t)
	pg.seedAdmins(t, map[int64]string{1001: RoleRiskManager})
	now := time.Now()
	tradeID, _, _, _ := pg.seedTrade(t, "1.15", "1.00", "1000", "2", "3",
		now.Add(-2*time.Minute), false)
	svc, _, _, _ := newBustService(t, pg,
		map[int64]string{1001: RoleRiskManager}, now)
	out, err := svc.RequestBust(context.Background(), AdminActor{UserID: 1001},
		BustRequest{TradeID: tradeID, Action: BustActionBust, Reason: "x"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	// Age the review past the 15-minute four-eyes window.
	if _, err := pg.pool.Exec(context.Background(),
		`UPDATE trade_busts SET created_at = now() - interval '20 minutes'
		  WHERE id=$1`, out.Bust.ID); err != nil {
		t.Fatalf("age: %v", err)
	}
	n, err := svc.ExpirePending(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("expire: n=%d err=%v", n, err)
	}
	held, _ := svc.HasPendingBust(context.Background(), tradeID)
	if held {
		t.Fatal("expired review must release the settlement hold")
	}
}

// ---------------------------------------------------------------------------
// Instrument maintenance — PG-gated workflow tests.
// ---------------------------------------------------------------------------

type maintFixture struct {
	svc    *InstrumentMaintenanceService
	dual   *DualControlService
	instr  *InstrumentService
	nats   *maintFakeNATS
	ws     *maintFakeWS
	alerts *maintFakeAlerter
	now    time.Time
}

type maintFakeNATS struct {
	subjects []string
	payloads [][]byte
	fail     bool
}

func (f *maintFakeNATS) Publish(_ context.Context, subject string, payload []byte) error {
	if f.fail {
		return fmt.Errorf("nats down")
	}
	f.subjects = append(f.subjects, subject)
	f.payloads = append(f.payloads, payload)
	return nil
}

type maintFakeWS struct {
	events []InstrumentStatusEvent
}

func (f *maintFakeWS) Publish(_ string, data any) {
	if ev, ok := data.(InstrumentStatusEvent); ok {
		f.events = append(f.events, ev)
	}
}

type maintFakeFeed struct {
	statuses map[string]string
	fail     bool
}

func (f *maintFakeFeed) SetStatus(_ context.Context, sym, st string) error {
	if f.fail {
		return fmt.Errorf("redis down")
	}
	f.statuses[sym] = st
	return nil
}
func (f *maintFakeFeed) GetStatus(_ context.Context, sym string) (string, error) {
	return f.statuses[sym], nil
}
func (f *maintFakeFeed) DelStatus(_ context.Context, sym string) error {
	delete(f.statuses, sym)
	return nil
}
func (f *maintFakeFeed) ScanStatuses(_ context.Context) (map[string]string, error) {
	return f.statuses, nil
}
func (f *maintFakeFeed) SetAuctionCall(_ context.Context, _ string, _ int64) error {
	return nil
}
func (f *maintFakeFeed) DelAuction(_ context.Context, _ string) error { return nil }
func (f *maintFakeFeed) SetSweepDeadline(_ context.Context, _ int64, _ time.Time) error {
	return nil
}
func (f *maintFakeFeed) DelSweepKeys(_ context.Context, _ int64) error { return nil }
func (f *maintFakeFeed) IsSwept(_ context.Context, _ int64) (bool, error) {
	return false, nil
}
func (f *maintFakeFeed) MarkSwept(_ context.Context, _ int64) error { return nil }

type maintFakeAlerter struct{ alerts []string }

func (f *maintFakeAlerter) Alert(_ context.Context, sev, _, msg string) error {
	f.alerts = append(f.alerts, sev+":"+msg)
	return nil
}

func maintPGFixture(t *testing.T, pg *bustPG, roles map[int64]string,
	now time.Time) *maintFixture {
	store := NewStore(pg.pool)
	instr, err := NewInstrumentService(InstrumentDeps{
		Pool: pg.pool, Roles: testRoles(roles), Feed: &maintFakeFeed{statuses: map[string]string{}},
	})
	if err != nil {
		t.Fatalf("instrument svc: %v", err)
	}
	dual := NewDualControlService(pg.pool, store)
	fix := &maintFixture{
		dual: dual, instr: instr,
		nats: &maintFakeNATS{}, ws: &maintFakeWS{}, alerts: &maintFakeAlerter{},
		now: now,
	}
	svc, err := NewInstrumentMaintenanceService(InstrumentMaintenanceDeps{
		Pool: pg.pool, Roles: testRoles(roles), Instruments: instr,
		Dual: dual, WS: fix.ws, NATS: fix.nats, Alerter: fix.alerts,
		Now: func() time.Time { return fix.now },
	})
	if err != nil {
		t.Fatalf("maintenance svc: %v", err)
	}
	fix.svc = svc
	return fix
}

var maintRoles = map[int64]string{
	1001: RoleRiskManager, 1002: RoleComplianceOfficer, 1003: RoleSuperAdmin,
	1004: RoleRiskManager, 1005: RoleSuperAdmin, 1006: RoleComplianceOfficer,
	1007: RoleSupportAgent,
}

func TestCreateMakerCheckerChainPG(t *testing.T) {
	pg := bustPGFixture(t)
	pg.seedAdmins(t, maintRoles)
	fix := maintPGFixture(t, pg, maintRoles, time.Now())

	spec := InstrumentCreate{
		Symbol: "GBP/JPY", InstrumentType: "SPOT", TickSize: "0.001",
		LotSize: "1000", MinOrderQty: "100", MaxOrderQty: "1000000",
		SettlementCycle: 2, MaxLeverage: 30, Reason: "listing GBP/JPY",
	}
	c, err := fix.svc.ProposeCreate(context.Background(), AdminActor{UserID: 1001}, spec)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if c.Stage != ChgPendingReview {
		t.Fatalf("stage %s want PENDING_REVIEW", c.Stage)
	}
	// Malformed symbol rejects before a row lands.
	if _, err := fix.svc.ProposeCreate(context.Background(), AdminActor{UserID: 1001},
		InstrumentCreate{Symbol: "GBPJPY", InstrumentType: "SPOT", Reason: "x"}); err == nil {
		t.Fatal("malformed symbol must reject")
	}
	// Wrong role proposes.
	if _, err := fix.svc.ProposeCreate(context.Background(), AdminActor{UserID: 1007}, spec); err == nil {
		t.Fatal("Support Agent must not propose")
	}
	// Proposer cannot self-review.
	if _, err := fix.svc.ReviewCreate(context.Background(), AdminActor{UserID: 1001}, c.ID, true, ""); err == nil {
		t.Fatal("self-review must fail")
	}
	// Compliance reviews → PENDING_APPROVAL.
	c2, err := fix.svc.ReviewCreate(context.Background(), AdminActor{UserID: 1002}, c.ID, true, "lgtm")
	if err != nil || c2.Stage != ChgPendingApproval {
		t.Fatalf("review: %v stage=%s", err, c2.Stage)
	}
	// Reviewer cannot also approve (three distinct principals).
	if _, _, err := fix.svc.ApproveCreate(context.Background(), AdminActor{UserID: 1002}, c.ID); err == nil {
		t.Fatal("reviewer must not approve — three distinct principals")
	}
	// Risk Manager cannot approve — Super Admin only.
	if _, _, err := fix.svc.ApproveCreate(context.Background(), AdminActor{UserID: 1004}, c.ID); err == nil {
		t.Fatal("Risk Manager must not approve")
	}
	// Super Admin approves → APPLIED + DRAFT instrument exists.
	c3, inst, err := fix.svc.ApproveCreate(context.Background(), AdminActor{UserID: 1003}, c.ID)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if c3.Stage != ChgApplied || inst.Status != InstDraft {
		t.Fatalf("want APPLIED + DRAFT, got %s/%s", c3.Stage, inst.Status)
	}
	if len(fix.nats.subjects) == 0 || fix.nats.subjects[0] != SecurityStatusSubject {
		t.Fatalf("security_status event expected, got %v", fix.nats.subjects)
	}
	// Immutable audit trail — who/what/when/why.
	var logCount int
	if err := pg.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM instrument_change_log WHERE request_id=$1`, c.ID).Scan(&logCount); err != nil {
		t.Fatalf("log count: %v", err)
	}
	if logCount < 3 {
		t.Fatalf("want ≥3 change-log rows (submit/review/apply), got %d", logCount)
	}
	// Immutability: UPDATE must fail closed.
	if _, err := pg.pool.Exec(context.Background(),
		`UPDATE instrument_change_log SET reason='tampered' WHERE request_id=$1`, c.ID); err == nil {
		t.Fatal("instrument_change_log UPDATE must be forbidden")
	}
	if _, err := pg.pool.Exec(context.Background(),
		`DELETE FROM instrument_change_log WHERE request_id=$1`, c.ID); err == nil {
		t.Fatal("instrument_change_log DELETE must be forbidden")
	}
}

func TestParamChangeScheduledPG(t *testing.T) {
	pg := bustPGFixture(t)
	pg.seedAdmins(t, maintRoles)
	now := time.Now()
	fix := maintPGFixture(t, pg, maintRoles, now)

	var instID int64
	err := pg.pool.QueryRow(context.Background(), `
		INSERT INTO instruments
		    (symbol, base_currency, quote_currency, instrument_type,
		     tick_size, lot_size, min_order_qty, max_order_qty,
		     settlement_cycle, max_leverage, status)
		VALUES ('USD/JPY','USD','JPY','SPOT',0.001,1000,100,1000000,1,30,'ACTIVE')
		RETURNING id`).Scan(&instID)
	if err != nil {
		t.Fatalf("instrument: %v", err)
	}

	c, err := fix.svc.ProposeParamChange(context.Background(), AdminActor{UserID: 1001},
		ParamChangeInput{InstrumentID: instID, Field: "tick_size",
			NewValue: "0.005", Reason: "wider ticks"})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	// Maker cannot check own change.
	if _, err := fix.svc.DecideParamChange(context.Background(),
		AdminActor{UserID: 1001}, c.ID, true, ""); err == nil {
		t.Fatal("self-check must fail")
	}
	// Concurrent second proposal on the same field → unique violation.
	if _, err := fix.svc.ProposeParamChange(context.Background(),
		AdminActor{UserID: 1004},
		ParamChangeInput{InstrumentID: instID, Field: "tick_size",
			NewValue: "0.01", Reason: "race"}); err == nil {
		t.Fatal("concurrent open change on same field must reject")
	}
	// Distinct checker approves → SCHEDULED for next session boundary.
	c2, err := fix.svc.DecideParamChange(context.Background(),
		AdminActor{UserID: 1004}, c.ID, true, "")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if c2.Stage != ChgScheduled || c2.EffectiveAt == nil {
		t.Fatalf("want SCHEDULED + effective_at, got %+v", c2)
	}
	// Not yet effective — column unchanged.
	var tick string
	_ = pg.pool.QueryRow(context.Background(),
		`SELECT tick_size::text FROM instruments WHERE id=$1`, instID).Scan(&tick)
	if strings.HasPrefix(tick, "0.005") {
		t.Fatalf("tick applied early: %s", tick)
	}
	// Due sweep before effective_at → nothing.
	if n, err := fix.svc.ApplyDue(context.Background()); err != nil || n != 0 {
		t.Fatalf("ApplyDue early: n=%d err=%v", n, err)
	}
	// Advance past effective_at → applies.
	if _, err := pg.pool.Exec(context.Background(),
		`UPDATE instrument_change_requests SET effective_at = now() - interval '1 second'
		  WHERE id=$1`, c.ID); err != nil {
		t.Fatalf("force due: %v", err)
	}
	if n, err := fix.svc.ApplyDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("ApplyDue: n=%d err=%v", n, err)
	}
	_ = pg.pool.QueryRow(context.Background(),
		`SELECT tick_size::text FROM instruments WHERE id=$1`, instID).Scan(&tick)
	if !strings.HasPrefix(tick, "0.005") {
		t.Fatalf("tick not applied: %s", tick)
	}
	// Non-column param lands in param_overrides.
	c3, err := fix.svc.ProposeParamChange(context.Background(), AdminActor{UserID: 1001},
		ParamChangeInput{InstrumentID: instID, Field: "margin_rate",
			NewValue: "0.05", Reason: "margin bump"})
	if err != nil {
		t.Fatalf("propose margin: %v", err)
	}
	if _, err := fix.svc.DecideParamChange(context.Background(),
		AdminActor{UserID: 1004}, c3.ID, true, ""); err != nil {
		t.Fatalf("decide margin: %v", err)
	}
	if _, err := pg.pool.Exec(context.Background(),
		`UPDATE instrument_change_requests SET effective_at = now() - interval '1 second'
		  WHERE id=$1`, c3.ID); err != nil {
		t.Fatalf("force margin due: %v", err)
	}
	if n, err := fix.svc.ApplyDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("ApplyDue margin: n=%d err=%v", n, err)
	}
	var mr string
	if err := pg.pool.QueryRow(context.Background(),
		`SELECT param_overrides->>'margin_rate' FROM instruments WHERE id=$1`,
		instID).Scan(&mr); err != nil || mr != "0.05" {
		t.Fatalf("param_overrides.margin_rate = %v err=%v", mr, err)
	}
}

func TestEmergencyParamChangePG(t *testing.T) {
	pg := bustPGFixture(t)
	pg.seedAdmins(t, maintRoles)
	fix := maintPGFixture(t, pg, maintRoles, time.Now())

	var instID int64
	err := pg.pool.QueryRow(context.Background(), `
		INSERT INTO instruments
		    (symbol, base_currency, quote_currency, instrument_type,
		     tick_size, lot_size, min_order_qty, max_order_qty,
		     settlement_cycle, max_leverage, status)
		VALUES ('USD/MXN','USD','MXN','SPOT',0.0001,1000,100,1000000,2,10,'ACTIVE')
		RETURNING id`).Scan(&instID)
	if err != nil {
		t.Fatalf("instrument: %v", err)
	}
	// Non-Super-Admin emergency → refused.
	if _, err := fix.svc.EmergencyParamChange(context.Background(),
		AdminActor{UserID: 1001},
		ParamChangeInput{InstrumentID: instID, Field: "max_leverage",
			NewValue: "5", Reason: "vol spike"}); err == nil {
		t.Fatal("Risk Manager emergency must refuse — Super Admin only")
	}
	// Super Admin → immediate apply + P1.
	c, err := fix.svc.EmergencyParamChange(context.Background(),
		AdminActor{UserID: 1003},
		ParamChangeInput{InstrumentID: instID, Field: "max_leverage",
			NewValue: "5", Reason: "vol spike — deleverage now"})
	if err != nil {
		t.Fatalf("emergency: %v", err)
	}
	if c.Stage != ChgApplied || !c.Emergency {
		t.Fatalf("want APPLIED+emergency, got %+v", c)
	}
	var lev int
	_ = pg.pool.QueryRow(context.Background(),
		`SELECT max_leverage FROM instruments WHERE id=$1`, instID).Scan(&lev)
	if lev != 5 {
		t.Fatalf("max_leverage %d want 5", lev)
	}
	if len(fix.alerts.alerts) != 1 || !strings.HasPrefix(fix.alerts.alerts[0], "P1:") {
		t.Fatalf("P1 alert required, got %v", fix.alerts.alerts)
	}
	var ev SecurityStatusEvent
	if len(fix.nats.payloads) == 0 {
		t.Fatal("security_status event expected")
	}
	if err := json.Unmarshal(fix.nats.payloads[len(fix.nats.payloads)-1], &ev); err != nil {
		t.Fatalf("event decode: %v", err)
	}
	if !ev.Emergency || ev.Field != "max_leverage" {
		t.Fatalf("event %+v must carry emergency + field", ev)
	}
}

func TestDelistWorkflowPG(t *testing.T) {
	pg := bustPGFixture(t)
	pg.seedAdmins(t, maintRoles)
	fix := maintPGFixture(t, pg, maintRoles, time.Now())

	var instID int64
	err := pg.pool.QueryRow(context.Background(), `
		INSERT INTO instruments
		    (symbol, base_currency, quote_currency, instrument_type,
		     tick_size, lot_size, min_order_qty, max_order_qty,
		     settlement_cycle, max_leverage, status)
		VALUES ('AUD/USD','AUD','USD','SPOT',0.0001,1000,100,1000000,1,30,'ACTIVE')
		RETURNING id`).Scan(&instID)
	if err != nil {
		t.Fatalf("instrument: %v", err)
	}
	// Open margin positions on the instrument — delisting must proceed
	// (the §7.5 30-day close-only ladder handles wind-down engine-side).
	if _, err := pg.pool.Exec(context.Background(),
		`INSERT INTO users (id) VALUES (901) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := pg.pool.Exec(context.Background(),
		`INSERT INTO accounts (user_id) VALUES (901)`); err != nil {
		t.Fatalf("account: %v", err)
	}
	if _, err := pg.pool.Exec(context.Background(), `
		INSERT INTO positions (account_id, instrument_id, side, quantity, entry_price)
		VALUES ((SELECT id FROM accounts WHERE user_id=901 LIMIT 1),$1,'LONG',500,0.65)`,
		instID); err != nil {
		t.Fatalf("position: %v", err)
	}
	// Register the real delist executor (the api package does this in
	// production — replicating it here keeps the admin test package-private).
	fix.dual.RegisterExecutor(OpInstrumentDelist,
		func(ctx context.Context, tx pgx.Tx, req *DualControlRequest) error {
			var p struct {
				InstrumentID int64 `json:"instrument_id"`
			}
			if err := json.Unmarshal(req.Payload, &p); err != nil {
				return err
			}
			actor := AdminActor{UserID: req.RequestedBy}
			if req.ApprovedBy != nil {
				actor.ApproverID = *req.ApprovedBy
			}
			return fix.instr.TransitionTx(ctx, tx, actor, p.InstrumentID, LcOpDelist,
				TransitionInput{Reason: req.Reason})
		})

	c, dc, err := fix.svc.RequestDelist(context.Background(), AdminActor{UserID: 1003},
		instID, "regulatory directive — cease trading")
	if err != nil {
		t.Fatalf("request delist: %v", err)
	}
	if c.ChangeType != ChangeDelist || c.Stage != ChgPendingApproval {
		t.Fatalf("delist request: %+v", c)
	}
	// Non-Super-Admin submit refuses.
	if _, _, err := fix.svc.RequestDelist(context.Background(), AdminActor{UserID: 1001},
		instID, "x"); err == nil {
		t.Fatal("Risk Manager must not submit delist")
	}
	// Approve via the dual-control queue (distinct SA).
	req, err := fix.dual.Approve(context.Background(), dc.ID, 1005, "127.0.0.1")
	if err != nil {
		t.Fatalf("dual approve: %v", err)
	}
	if req.Status != ReqExecuted {
		t.Fatalf("dc status %s want EXECUTED", req.Status)
	}
	var st string
	_ = pg.pool.QueryRow(context.Background(),
		`SELECT status::text FROM instruments WHERE id=$1`, instID).Scan(&st)
	if st != InstDelisted {
		t.Fatalf("instrument status %s want DELISTED", st)
	}
	// Sync → APPLIED + change-log APPLIED row + security_status event.
	if err := fix.svc.SyncDelistRequests(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	c2, err := fix.svc.GetChange(context.Background(), c.ID)
	if err != nil || c2.Stage != ChgApplied {
		t.Fatalf("delist change stage %v want APPLIED (%v)", c2.Stage, err)
	}
	var applied int
	_ = pg.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM instrument_change_log
		  WHERE request_id=$1 AND action='APPLIED'`, c.ID).Scan(&applied)
	if applied != 1 {
		t.Fatalf("want APPLIED log row, got %d", applied)
	}
	var ev SecurityStatusEvent
	found := false
	for _, p := range fix.nats.payloads {
		if json.Unmarshal(p, &ev) == nil && ev.Status == InstDelisted {
			found = true
		}
	}
	if !found {
		t.Fatal("DELISTED security_status event must emit")
	}
	// Open position survives — close-only handling is the lifecycle engine's.
	var qty decimal.Decimal
	_ = pg.pool.QueryRow(context.Background(),
		`SELECT quantity FROM positions WHERE instrument_id=$1`, instID).Scan(&qty)
	if !qty.Equal(dec("500")) {
		t.Fatalf("open position must survive delisting, got %s", qty)
	}
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	var e *excerrors.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("expected code %s, got %v", code, err)
	}
}
