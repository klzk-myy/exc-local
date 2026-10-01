// PG-gated integration coverage for Task 16.3.9's benchmark-fixing
// residual leg — runs only when EXC_PG_TEST=1.
//
//	EXC_PG_TEST=1 go test ./internal/algo/ -run TestITFixing -v
//
// Spec §6.4 / AC 16.7: "Imbalances cleared with zero tracking error vs
// official published benchmark." The designated institutional-LP leg is
// an ordinary FIXING order on the opposite side — the executor crosses
// it in the same SERIALIZABLE transaction at the exact published rate,
// so a residual can never clear at a tracked-error price and no
// synthetic fill is ever injected. These tests prove that contract end
// to end against a real database: client residual absorbed by LP,
// client fully FILLED at the recorded benchmark rate, LP remainder
// honestly audited as FIXING_IMBALANCE, and the no-LP case leaving the
// residual queued (imbalance recorded, nothing fabricated).
//
// Fixture mirrors the read/write projections the executor touches
// (orders/trades/order_audit/benchmark_fixings/instruments/accounts);
// the accounting spine (004 balances, 036 GL, 088 chart seeds, 102
// wallet shadow) is applied from the real migrations verbatim.
package algo

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/instruments"
	"exchange/internal/ledger"
	"exchange/internal/settlement"
	"exchange/pkg/decimal"
)

// fixingCal satisfies CalendarSource — Tick never consults the calendar
// (only admission does), so an empty answer is honest here.
type fixingCal struct{}

func (fixingCal) CalendarFor(context.Context, string) ([]instruments.CalendarEntry, error) {
	return nil, nil
}

const fixingITFixture = `
CREATE TYPE order_status_enum AS ENUM (
    'PENDING', 'RESERVED', 'ACTIVE', 'PARTIALLY_FILLED',
    'FILLED', 'CANCELLED', 'REJECTED', 'EXPIRED');

CREATE TABLE accounts (id BIGSERIAL PRIMARY KEY);

CREATE TABLE instruments (
    id             BIGSERIAL PRIMARY KEY,
    symbol         VARCHAR(32) UNIQUE NOT NULL,
    base_currency  VARCHAR(3)  NOT NULL,
    quote_currency VARCHAR(3)  NOT NULL);

CREATE TABLE orders (
    id               BIGSERIAL PRIMARY KEY,
    account_id       BIGINT            NOT NULL,
    instrument_id    BIGINT            NOT NULL REFERENCES instruments (id),
    side             VARCHAR(4)        NOT NULL,
    order_type       VARCHAR(16)       NOT NULL,
    quantity         DECIMAL(28,8)     NOT NULL,
    filled_qty       DECIMAL(28,8)     NOT NULL DEFAULT 0,
    avg_fill_price   DECIMAL(20,8),
    status           order_status_enum NOT NULL DEFAULT 'PENDING',
    fixing_benchmark VARCHAR(32),
    algo_params      JSONB,
    shard_id         SMALLINT,
    created_at       TIMESTAMPTZ       NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ       NOT NULL DEFAULT now());

CREATE TABLE trades (
    id                 BIGSERIAL PRIMARY KEY,
    instrument_id      BIGINT NOT NULL,
    buy_order_id       BIGINT NOT NULL,
    sell_order_id      BIGINT NOT NULL,
    buyer_account_id   BIGINT NOT NULL,
    seller_account_id  BIGINT NOT NULL,
    price              DECIMAL(20,8) NOT NULL,
    quantity           DECIMAL(28,8) NOT NULL,
    buyer_fee          DECIMAL(28,8),
    seller_fee         DECIMAL(28,8),
    settlement_date    DATE,
    shard_id           SMALLINT,
    trade_seq          BIGINT);

CREATE TABLE order_audit (
    id          BIGSERIAL PRIMARY KEY,
    order_id    BIGINT NOT NULL,
    account_id  BIGINT NOT NULL,
    operation   VARCHAR(32) NOT NULL,
    field_name  VARCHAR(64),
    old_value   TEXT,
    new_value   TEXT,
    modified_by VARCHAR(64),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now());

CREATE TABLE benchmark_fixings (
    id            BIGSERIAL PRIMARY KEY,
    instrument_id BIGINT NOT NULL,
    symbol        VARCHAR(32) NOT NULL,
    benchmark     VARCHAR(32) NOT NULL,
    scheduled_at  TIMESTAMPTZ NOT NULL,
    fired_at      TIMESTAMPTZ,
    rate          DECIMAL(20,8),
    rate_source   VARCHAR(64),
    status        VARCHAR(16) NOT NULL,
    skip_reason   VARCHAR(255),
    order_ids     JSONB);

INSERT INTO accounts (id) VALUES (10), (20);
INSERT INTO instruments (id, symbol, base_currency, quote_currency)
VALUES (1, 'EUR/USD', 'EUR', 'USD');`

// fixingIT seeds the fixture + real accounting-spine migrations and
// returns a ready FixingService.
func fixingIT(t *testing.T) (context.Context, *pgxpool.Pool, *FixingService,
	*settlement.LedgerService) {
	t.Helper()
	ctx, pool := itPool(t)
	if _, err := pool.Exec(ctx, fixingITFixture); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	// Accounting spine — real migrations, verbatim: balances (004),
	// GL + zero-sum constraint trigger (036), chart seeds incl.
	// 2160_CLEARING_TRANSIT (088), wallet shadow (102).
	for _, mig := range []string{
		"004_create_balances.up.sql",
		"036_create_general_ledger.up.sql",
		"088_gl_chart_of_accounts.up.sql",
		"102_ledger_wallet_shadow.up.sql",
	} {
		execSQL(t, ctx, pool, mig)
	}
	led, err := settlement.NewLedgerService(pool, nil, nil)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	svc, err := NewFixingService(FixingDeps{
		Pool: pool, Poster: led, TxPoster: led, Calendar: fixingCal{},
	})
	if err != nil {
		t.Fatalf("fixing service: %v", err)
	}
	return ctx, pool, svc, led
}

// insertFixingOrder writes one RESERVED FIXING order with its
// fix-time reservation record already merged into algo_params.
func insertFixingOrder(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	account int64, side, qty, rsvCurrency, rsvAmount string, createdAt time.Time) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO orders (account_id, instrument_id, side, order_type,
		    quantity, status, fixing_benchmark, algo_params, created_at)
		VALUES ($1, 1, $2, 'FIXING', $3::numeric, 'RESERVED', 'WM_R_4PM',
		    jsonb_build_object('fixing_reservation', jsonb_build_object(
		        'currency', $4::text, 'amount', $5::text, 'consumed', '0')),
		    $6)
		RETURNING id`,
		account, side, qty, rsvCurrency, rsvAmount, createdAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert fixing order: %v", err)
	}
	return id
}

func insertRecordedFixing(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	rate string, scheduledAt time.Time) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO benchmark_fixings (instrument_id, symbol, benchmark,
		    scheduled_at, fired_at, rate, rate_source, status)
		VALUES (1, 'EUR/USD', 'WM_LONDON_4PM', $1, now(), $2::numeric,
		    'oracle-test', 'RECORDED')
		RETURNING id`, scheduledAt, rate).Scan(&id)
	if err != nil {
		t.Fatalf("insert fixing: %v", err)
	}
	return id
}

// fundAndReserve posts the two journals every FIXING reservation stands
// on in production — a deposit (available credit) then the reservation
// shift (available → locked, client liability → clearing transit). Going
// through PostJournal keeps ledger_entries/journal_sums history
// consistent so the zero-sum wallet assertion sees real provenance
// rather than a hand-seeded balance.
func fundAndReserve(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	led *settlement.LedgerService, account int64, currency, amount string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("seed tx: %v", err)
	}
	defer tx.Rollback(ctx)
	amt := decimal.RequireFromString(amount)
	key := fmt.Sprintf("it-seed:%d:%s", account, currency)
	if _, err := led.PostJournal(ctx, tx, ledger.Journal{
		EntryType:      ledger.EntryDeposit,
		Description:    "itest deposit",
		PostedBy:       "itest",
		IdempotencyKey: key + ":dep",
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.Nostro(currency), currency, amt, "deposit in"),
			ledger.CreditLine(ledger.CustomerLiability(currency), currency, amt, "deposit owed"),
		},
		Effects: []ledger.AccountEffect{
			{AccountID: account, Currency: currency, AvailableDelta: amt},
		},
	}); err != nil {
		t.Fatalf("seed deposit: %v", err)
	}
	if _, err := led.PostJournal(ctx, tx, ledger.Journal{
		EntryType:      ledger.EntryAdjustment,
		Description:    "itest reservation",
		PostedBy:       "itest",
		IdempotencyKey: key + ":rsv",
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability(currency), currency, amt, "reserve out"),
			ledger.CreditLine(ledger.ClearingTransit(currency), currency, amt, "reserve held"),
		},
		Effects: []ledger.AccountEffect{
			{AccountID: account, Currency: currency,
				AvailableDelta: amt.Neg(), LockedDelta: amt},
		},
	}); err != nil {
		t.Fatalf("seed reserve: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
}

// Residual imbalance absorbed by a designated LP's FIXING order: client
// buy 1000 vs LP sell 1500 at the published rate — client FILLED with
// zero tracking error, LP residual queued and audited, every ledger /
// trade / order figure conserved.
func TestITFixingLPResidualCleared(t *testing.T) {
	ctx, pool, svc, led := fixingIT(t)

	const rate = "1.08525000"
	scheduled := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	preCutoff := scheduled.Add(-30 * time.Minute) // inside T-15 eligibility

	// Client buy 1000 EUR → quote obligation 1000 × 1.08525 = 1085.25 USD.
	fundAndReserve(t, ctx, pool, led, 10, "USD", "1085.25")
	buyID := insertFixingOrder(t, ctx, pool, 10, "BUY", "1000", "USD", "1085.25", preCutoff)
	// Designated LP sell 1500 EUR — deeper than the client residual.
	fundAndReserve(t, ctx, pool, led, 20, "EUR", "1500")
	sellID := insertFixingOrder(t, ctx, pool, 20, "SELL", "1500", "EUR", "1500", preCutoff)

	fixID := insertRecordedFixing(t, ctx, pool, rate, scheduled)

	fills, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if fills != 1 {
		t.Fatalf("fills=%d want 1", fills)
	}

	// Zero tracking error: every fill prints at exactly the published rate.
	var price string
	if err := pool.QueryRow(ctx,
		`SELECT price::text FROM trades WHERE buy_order_id=$1 AND sell_order_id=$2`,
		buyID, sellID).Scan(&price); err != nil {
		t.Fatalf("trade: %v", err)
	}
	if !decimal.RequireFromString(price).Equal(decimal.RequireFromString(rate)) {
		t.Fatalf("tracking error: trade price %s != fix %s", price, rate)
	}

	// Client residual fully cleared.
	var st, fq, afp string
	if err := pool.QueryRow(ctx,
		`SELECT status::text, filled_qty::text, avg_fill_price::text
		 FROM orders WHERE id=$1`, buyID).Scan(&st, &fq, &afp); err != nil {
		t.Fatalf("buy order: %v", err)
	}
	if st != "FILLED" || fq != "1000.00000000" {
		t.Fatalf("buy order status=%s filled=%s", st, fq)
	}
	if !decimal.RequireFromString(afp).Equal(decimal.RequireFromString(rate)) {
		t.Fatalf("avg_fill_price %s != fix %s", afp, rate)
	}

	// LP partially filled (1000 of 1500); its 500 remainder is the
	// honestly-recorded imbalance.
	if err := pool.QueryRow(ctx,
		`SELECT status::text, filled_qty::text FROM orders WHERE id=$1`,
		sellID).Scan(&st, &fq); err != nil {
		t.Fatalf("lp order: %v", err)
	}
	if st != "PARTIALLY_FILLED" || fq != "1000.00000000" {
		t.Fatalf("lp order status=%s filled=%s", st, fq)
	}
	var imbLP, imbClient bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM order_audit WHERE order_id=$1
		   AND operation='FIXING_IMBALANCE' AND new_value LIKE $2 || '%')`,
		sellID, fmt.Sprint(fixID)).Scan(&imbLP); err != nil {
		t.Fatalf("lp imbalance: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM order_audit WHERE order_id=$1
		   AND operation='FIXING_IMBALANCE')`, buyID).Scan(&imbClient); err != nil {
		t.Fatalf("client imbalance: %v", err)
	}
	if !imbLP || imbClient {
		t.Fatalf("imbalance audit wrong: lp=%v client=%v", imbLP, imbClient)
	}

	// Wallet conservation: locked reservations consumed, proceeds landed.
	var avail, locked string
	if err := pool.QueryRow(ctx,
		`SELECT available::text, locked::text FROM balances
		 WHERE account_id=10 AND currency='USD'`).Scan(&avail, &locked); err != nil {
		t.Fatalf("client usd: %v", err)
	}
	if avail != "0.00000000" || locked != "0.00000000" {
		t.Fatalf("client USD avail=%s locked=%s", avail, locked)
	}
	if err := pool.QueryRow(ctx,
		`SELECT available::text FROM balances WHERE account_id=10 AND currency='EUR'`).
		Scan(&avail); err != nil {
		t.Fatalf("client eur: %v", err)
	}
	if avail != "1000.00000000" {
		t.Fatalf("client EUR avail=%s", avail)
	}
	if err := pool.QueryRow(ctx,
		`SELECT locked::text FROM balances WHERE account_id=20 AND currency='EUR'`).
		Scan(&locked); err != nil {
		t.Fatalf("lp eur: %v", err)
	}
	if locked != "500.00000000" {
		t.Fatalf("lp EUR locked=%s (residual must stay reserved)", locked)
	}
	if err := pool.QueryRow(ctx,
		`SELECT available::text FROM balances WHERE account_id=20 AND currency='USD'`).
		Scan(&avail); err != nil {
		t.Fatalf("lp usd: %v", err)
	}
	if avail != "1085.25000000" {
		t.Fatalf("lp USD avail=%s", avail)
	}
}

// No LP on the opposite side: the client order stays queued, the
// imbalance is recorded, and nothing is fabricated.
func TestITFixingResidualWithoutLPHonest(t *testing.T) {
	ctx, pool, svc, led := fixingIT(t)

	scheduled := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	preCutoff := scheduled.Add(-30 * time.Minute)

	fundAndReserve(t, ctx, pool, led, 10, "USD", "1085.25")
	buyID := insertFixingOrder(t, ctx, pool, 10, "BUY", "1000", "USD", "1085.25", preCutoff)
	fixID := insertRecordedFixing(t, ctx, pool, "1.08525000", scheduled)

	fills, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if fills != 0 {
		t.Fatalf("fills=%d want 0 (no counterparty)", fills)
	}
	var st string
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM orders WHERE id=$1`, buyID).Scan(&st); err != nil {
		t.Fatalf("order: %v", err)
	}
	if st != "RESERVED" {
		t.Fatalf("order status=%s — residual must stay queued", st)
	}
	var imb bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM order_audit WHERE order_id=$1
		   AND operation='FIXING_IMBALANCE' AND new_value LIKE $2 || '%')`,
		buyID, fmt.Sprint(fixID)).Scan(&imb); err != nil {
		t.Fatalf("imbalance: %v", err)
	}
	if !imb {
		t.Fatal("FIXING_IMBALANCE audit missing for unmatched residual")
	}
	var tradeCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM trades`).Scan(&tradeCount); err != nil {
		t.Fatalf("trades: %v", err)
	}
	if tradeCount != 0 {
		t.Fatalf("fabricated fill: %d trades", tradeCount)
	}
}
