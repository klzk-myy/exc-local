// lifecycle_pg_test.go — PostgreSQL-gated coverage for the Task 22.3.10
// option lifecycle and the 255-migration roll tables: real SERIALIZABLE
// transactions, the GL/double-entry premium path, auto-exercise, OTM
// expiry, do-not-exercise, pro-rata writer assignment, and expiry-run
// idempotency. Gated on EXC_PG_TEST=1 + EXC_TEST_DSN (per pg_test.go).
package derivatives

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/oracle"
	"exchange/internal/oracle/rates"
	"exchange/internal/risk"
	"exchange/internal/settlement"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fixture seams
// ---------------------------------------------------------------------------

// noopLocker satisfies AccountLocker for single-connection tests — the
// §5.3 row locks (SELECT … FOR UPDATE) carry correctness here; the
// distributed mutex is exercised through RedisAccountLocker in deploy.
type noopLocker struct{}

func (noopLocker) LockAccounts(_ context.Context, ids []int64, _ string) ([]int64, error) {
	out := append([]int64(nil), ids...)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}
func (noopLocker) UnlockAccounts([]int64, string) {}

type lifecycleFakePub struct {
	subjects []string
	err      error
}

func (f *lifecycleFakePub) Publish(_ context.Context, subject string, _ []byte) error {
	if f.err != nil {
		return f.err
	}
	f.subjects = append(f.subjects, subject)
	return nil
}

type fakeMarginCalls struct{ queued []int64 }

func (f *fakeMarginCalls) QueuePremiumShortfall(_ context.Context, _, settlementID int64,
	_ string, _ decimal.Decimal) error {
	f.queued = append(f.queued, settlementID)
	return nil
}

// testClock is the mutable lifecycle clock — registration validates
// expiry against s.now(), so tests seed at trade time then advance to
// the expiry day.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time  { return c.t }
func (c *testClock) set(t time.Time) { c.t = t }

// pgMigExec applies a migration verbatim on a simple-protocol conn —
// extended-protocol Exec cannot carry multi-statement scripts.
func pgMigExec(t *testing.T, ctx context.Context, dsn, schema, file string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema
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

// pgExecList runs DDL fragments (split on ';') on a search_path-bound
// conn — same trick for fixture DDL.
func pgExecList(t *testing.T, ctx context.Context, dsn, schema, ddl string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	defer conn.Close(ctx)
	for _, stmt := range strings.Split(ddl, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("fixture DDL %q: %v", stmt[:min(60, len(stmt))], err)
		}
	}
}

const lifecycleAnchorDDL = `
CREATE TABLE accounts (id BIGSERIAL PRIMARY KEY);
CREATE TABLE instruments (
    id BIGSERIAL PRIMARY KEY,
    symbol VARCHAR(32) UNIQUE,
    instrument_type VARCHAR(16) NOT NULL,
    base_currency VARCHAR(3) NOT NULL,
    quote_currency VARCHAR(3) NOT NULL,
    settlement_cycle INT NOT NULL DEFAULT 1,
    max_leverage BIGINT NOT NULL DEFAULT 1,
    status VARCHAR(16) NOT NULL DEFAULT 'ACTIVE');
CREATE TYPE position_side_enum AS ENUM ('LONG','SHORT');
CREATE TABLE positions (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES accounts (id),
    instrument_id BIGINT NOT NULL REFERENCES instruments (id),
    side position_side_enum NOT NULL,
    quantity DECIMAL(28,8) NOT NULL,
    entry_price DECIMAL(28,8) NOT NULL DEFAULT 0,
    mark_price DECIMAL(28,8),
    unrealized_pnl DECIMAL(28,8) NOT NULL DEFAULT 0,
    realized_pnl DECIMAL(28,8) NOT NULL DEFAULT 0,
    margin_used DECIMAL(28,8) NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (account_id, instrument_id, side));
CREATE TABLE position_fills (
    trade_id BIGINT NOT NULL,
    account_id BIGINT NOT NULL,
    instrument_id BIGINT NOT NULL,
    side position_side_enum NOT NULL,
    quantity DECIMAL(28,8) NOT NULL,
    price DECIMAL(28,8) NOT NULL,
    realized_pnl DECIMAL(28,8) NOT NULL DEFAULT 0,
    PRIMARY KEY (trade_id, account_id));
CREATE TYPE settlement_direction_enum AS ENUM ('PAY','RECEIVE');
CREATE TYPE settlement_status_enum AS ENUM ('PENDING','SETTLED','FAILED','RECONCILED');
CREATE TABLE settlement_instructions (
    id BIGSERIAL PRIMARY KEY,
    trade_id BIGINT NOT NULL,
    account_id BIGINT NOT NULL,
    currency VARCHAR(3) NOT NULL,
    amount DECIMAL(28,8) NOT NULL,
    direction settlement_direction_enum NOT NULL,
    settlement_date DATE,
    status settlement_status_enum NOT NULL DEFAULT 'PENDING',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at TIMESTAMPTZ);
CREATE UNIQUE INDEX settlement_instructions_leg_ux
    ON settlement_instructions (trade_id, account_id, currency, direction);
CREATE TABLE balances (
    account_id BIGINT NOT NULL,
    currency VARCHAR(3) NOT NULL,
    available DECIMAL(28,8) NOT NULL DEFAULT 0,
    locked DECIMAL(28,8) NOT NULL DEFAULT 0,
    version BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, currency));
CREATE TYPE ledger_direction_enum AS ENUM ('DEBIT','CREDIT');
CREATE TYPE ledger_entry_type_enum AS ENUM
    ('DEPOSIT','WITHDRAWAL','TRADE_FILL','FEE','TRANSFER','SETTLEMENT','ROLLOVER','LIQUIDATION','ADJUSTMENT');
CREATE TABLE ledger_entries (
    id BIGSERIAL PRIMARY KEY,
    entry_type ledger_entry_type_enum NOT NULL,
    reference_id BIGINT,
    account_id BIGINT NOT NULL,
    currency VARCHAR(3) NOT NULL,
    direction ledger_direction_enum NOT NULL,
    amount DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    running_balance DECIMAL(28,8) NOT NULL,
    description VARCHAR(255),
    journal_entry_id BIGINT,
    posted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    posted_by VARCHAR(64));
CREATE TABLE journal_sums (
    account_id BIGINT NOT NULL,
    currency VARCHAR(3) NOT NULL,
    total_debits DECIMAL(28,8) NOT NULL DEFAULT 0,
    total_credits DECIMAL(28,8) NOT NULL DEFAULT 0,
    net_balance DECIMAL(28,8) GENERATED ALWAYS AS (total_debits - total_credits) STORED,
    entry_count BIGINT NOT NULL DEFAULT 0,
    last_entry_id BIGINT,
    PRIMARY KEY (account_id, currency));
CREATE SEQUENCE audit_hash_chain_id_seq;
CREATE TABLE audit_hash_chain (
    id BIGINT PRIMARY KEY,
    sequence_num BIGINT NOT NULL UNIQUE,
    table_name VARCHAR(64) NOT NULL,
    record_id BIGINT,
    action VARCHAR(16) NOT NULL,
    payload_hash VARCHAR(64) NOT NULL,
    prev_hash VARCHAR(64),
    created_at TIMESTAMPTZ NOT NULL)`

// lifecycleFixture builds the scratch schema + migrations and returns a
// search_path-bound pool.
func lifecycleFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run postgres-gated tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	boot, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	schema := fmt.Sprintf("opt_it_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
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

	pgExecList(t, ctx, dsn, schema, lifecycleAnchorDDL)
	migDir := filepath.Join("..", "db", "migrations")
	pgMigExec(t, ctx, dsn, schema, filepath.Join(migDir, "036_create_general_ledger.up.sql"))
	pgMigExec(t, ctx, dsn, schema, filepath.Join(migDir, "254_derivative_contracts.up.sql"))
	pgMigExec(t, ctx, dsn, schema, filepath.Join(migDir, "255_derivatives_roll_lifecycle.up.sql"))

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

// lifecycleSvc wires an OptionService on the fixture pool.
func lifecycleSvc(t *testing.T, pool *pgxpool.Pool, mark string,
	opts ...OptionServiceOption) (*OptionService, *lifecycleFakePub) {
	t.Helper()
	pub := &lifecycleFakePub{}
	ls, err := settlement.NewLedgerService(pool, nil, pub)
	if err != nil {
		t.Fatalf("ledger service: %v", err)
	}
	spot := fakeSpot{m: map[string]oracle.MarkView{
		"EURUSD": {Price: decimal.RequireFromString(mark), ValidAt: time.Now()},
	}}
	svc, err := NewOptionService(pool, ls, spot, noopLocker{}, pub, opts...)
	if err != nil {
		t.Fatalf("option service: %v", err)
	}
	return svc, pub
}

// seedAccount inserts an account + an optional USD balance. The funded
// path also plants the §5.3 journal_sums row — the ledger poster asserts
// net_balance == balances.total after every journal, so a directly-seeded
// balance must be ledger-tracked from its opening amount.
func seedAccount(t *testing.T, pool *pgxpool.Pool, id int64, balance string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO accounts (id) VALUES ($1)`, id); err != nil {
		t.Fatalf("account %d: %v", id, err)
	}
	if balance == "" {
		return
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO balances (account_id, currency, available, locked) VALUES ($1,'USD',$2,0)`,
		id, balance); err != nil {
		t.Fatalf("balance %d: %v", id, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO journal_sums (account_id, currency, total_debits, total_credits, entry_count)
		VALUES ($1,'USD',$2,0,1)`, id, balance); err != nil {
		t.Fatalf("journal_sums %d: %v", id, err)
	}
}

func avail(t *testing.T, pool *pgxpool.Pool, accountID int64, ccy string) decimal.Decimal {
	t.Helper()
	var s string
	err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(available,0)::text FROM balances WHERE account_id=$1 AND currency=$2`,
		accountID, ccy).Scan(&s)
	if err != nil {
		return decimal.Zero
	}
	return decimal.RequireFromString(s)
}

// seedOptionBook creates the OPTION + SPOT instruments, holder and
// writer positions rows, and registers both sides through the service.
func seedOptionBook(t *testing.T, pool *pgxpool.Pool, svc *OptionService,
	holderAcct, writerAcct int64, optType, settlementMode string,
	strike, qty, premium string, expiry, tradeDate time.Time) (holderOptID, writerOptID int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO instruments (id, symbol, instrument_type, base_currency, quote_currency, max_leverage)
		VALUES (701,'EURUSD_OPT','OPTION','EUR','USD',30),
		       (702,'EURUSD','SPOT','EUR','USD',30)
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("instruments: %v", err)
	}
	var holdPos, writePos int64
	err := pool.QueryRow(ctx, `
		INSERT INTO positions (account_id, instrument_id, side, quantity, entry_price)
		VALUES ($1,701,'LONG',$2,$3) RETURNING id`,
		holderAcct, qty, strike).Scan(&holdPos)
	if err != nil {
		t.Fatalf("holder position: %v", err)
	}
	err = pool.QueryRow(ctx, `
		INSERT INTO positions (account_id, instrument_id, side, quantity, entry_price)
		VALUES ($1,701,'SHORT',$2,$3) RETURNING id`,
		writerAcct, qty, strike).Scan(&writePos)
	if err != nil {
		t.Fatalf("writer position: %v", err)
	}
	reg := func(pos, acct, wPos, wAcct int64) int64 {
		id, err := svc.RegisterOptionPosition(ctx, OptionRegistration{
			PositionID: pos, AccountID: acct,
			WriterPositionID: wPos, WriterAccountID: wAcct,
			UnderlyingInstrumentID: 702, // EURUSD spot — the ITM mark symbol
			OptionType:             optType, ExerciseStyle: "EUROPEAN",
			Settlement:      settlementMode,
			Strike:          decimal.RequireFromString(strike),
			Quantity:        decimal.RequireFromString(qty),
			ExpiryAt:        expiry,
			Premium:         decimal.RequireFromString(premium),
			PremiumCurrency: "USD",
			TradeDate:       tradeDate,
		})
		if err != nil {
			t.Fatalf("register acct=%d: %v", acct, err)
		}
		return id
	}
	holderOptID = reg(holdPos, holderAcct, writePos, writerAcct)
	writerOptID = reg(writePos, writerAcct, 0, 0)
	return holderOptID, writerOptID
}

func optStatus(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT status::text FROM option_positions WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatalf("opt %d status: %v", id, err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Roll (migration 255 store path)
// ---------------------------------------------------------------------------

func TestPgxRollAtomicAndIdempotent(t *testing.T) {
	pool := lifecycleFixture(t)
	ctx := context.Background()
	seedAccount(t, pool, 7, "")
	store := NewPgxRollStore(pool)
	cal := testCalendar(t)
	d := NewDates(cal)
	now := day(2026, 1, 7)
	curves := fakeCurves{m: map[string]rates.Curve{
		"EUR": completeCurve("EUR", 0.04, now),
		"USD": completeCurve("USD", 0.05, now),
	}}
	spot := fakeSpot{m: map[string]oracle.MarkView{
		"EUR/USD": {Price: decimal.RequireFromString("1.10")},
	}}
	svc := NewRollService(store, d, NewPricer(curves, spot))
	svc.SetNowFunc(func() time.Time { return day(2026, 2, 6).Add(10 * time.Hour) })

	if _, err := pool.Exec(ctx, `
		INSERT INTO instruments (id, symbol, instrument_type, base_currency, quote_currency, max_leverage)
		VALUES (42,'EURUSD','SPOT','EUR','USD',30)`); err != nil {
		t.Fatalf("instrument: %v", err)
	}

	// Seed an open forward through the real store.
	var srcID int64
	err := store.InTx(ctx, func(ctx context.Context, tx RollTx) error {
		var err error
		srcID, _, err = tx.InsertContract(ctx, &Contract{
			TradeID: 9001, AccountID: 7, InstrumentID: 42,
			Kind: KindForward, Side: SideBuy,
			Pair:          Pair{Base: "EUR", Quote: "USD"},
			Notional:      decimal.RequireFromString("1000000"),
			SpotRate:      decimal.RequireFromString("1.09"),
			ForwardRate:   decimal.RequireFromString("1.095"),
			SwapPoints:    decimal.RequireFromString("0.005"),
			SpotValueDate: day(2026, 1, 9), ValueDate: day(2026, 2, 10),
			Status: StatusOpen, BookedAt: day(2026, 1, 7),
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed contract: %v", err)
	}

	res, err := svc.Roll(ctx, RollRequest{
		AccountID: 7, ContractID: srcID, Tenor: "1M", IdempotencyKey: "pg-roll-1",
	})
	if err != nil {
		t.Fatalf("roll: %v", err)
	}
	if res.Status != "COMPLETED" || res.Replayed {
		t.Fatalf("unexpected %+v", res)
	}
	var srcStatus string
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM derivative_contracts WHERE id=$1`, srcID).Scan(&srcStatus); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if srcStatus != string(StatusRolled) {
		t.Fatalf("source status %s", srcStatus)
	}
	var tgtStatus, tgtKind string
	err = pool.QueryRow(ctx, `
		SELECT status::text, kind::text FROM derivative_contracts WHERE id=$1`,
		res.TargetContractID).Scan(&tgtStatus, &tgtKind)
	if err != nil || tgtStatus != string(StatusOpen) || tgtKind != string(KindForward) {
		t.Fatalf("target contract status=%s kind=%s err=%v", tgtStatus, tgtKind, err)
	}
	var rollPrice string
	err = pool.QueryRow(ctx,
		`SELECT roll_price::text FROM contract_rolls WHERE id=$1`, res.RollID).Scan(&rollPrice)
	if err != nil || decimal.RequireFromString(rollPrice).IsZero() {
		t.Fatalf("roll_price not persisted: %q err=%v", rollPrice, err)
	}
	// Idempotent replay — the rolled source must not re-roll.
	res2, err := svc.Roll(ctx, RollRequest{
		AccountID: 7, ContractID: srcID, Tenor: "1M", IdempotencyKey: "pg-roll-1",
	})
	if err != nil || !res2.Replayed || res2.RollID != res.RollID {
		t.Fatalf("replay: %+v err=%v", res2, err)
	}
	// Audit chain captured the roll.
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM audit_hash_chain`).Scan(&n)
	if n == 0 {
		t.Fatalf("no audit rows")
	}
}

// ---------------------------------------------------------------------------
// Premium settlement (GL path)
// ---------------------------------------------------------------------------

func TestPgxPremiumSettlesThroughGL(t *testing.T) {
	pool := lifecycleFixture(t)
	ctx := context.Background()
	seedAccount(t, pool, 11, "10000")      // holder — funded
	seedAccount(t, pool, 12, "0")          // writer
	clk := &testClock{t: day(2026, 2, 25)} // trade day
	svc, pub := lifecycleSvc(t, pool, "1.1055", WithOptionClock(clk.now))

	expiry := day(2026, 3, 6).Add(10 * time.Hour) // Friday
	trade := day(2026, 2, 25)                     // Wednesday → due Friday
	holdID, _ := seedOptionBook(t, pool, svc, 11, 12, "CALL", SettlementCash,
		"1.10", "10000", "250", expiry, trade)

	// premium_due_date = trade + T+2 business days.
	var due string
	err := pool.QueryRow(ctx,
		`SELECT premium_due_date::text FROM option_positions WHERE id=$1`, holdID).Scan(&due)
	if err != nil {
		t.Fatalf("due date: %v", err)
	}
	if !strings.HasPrefix(due, "2026-02-27") {
		t.Fatalf("T+2 due date wrong: %s", due)
	}

	clk.set(day(2026, 2, 28)) // past due
	settled, failed, err := svc.RunPremiumSweep(ctx)
	if err != nil || settled != 1 || failed != 0 {
		t.Fatalf("sweep settled=%d failed=%d err=%v", settled, failed, err)
	}
	// Holder debited, writer credited in premium_currency (USD).
	if got := avail(t, pool, 11, "USD"); !got.Equal(decimal.RequireFromString("9750")) {
		t.Fatalf("holder balance %s, want 9750", got)
	}
	if got := avail(t, pool, 12, "USD"); !got.Equal(decimal.RequireFromString("250")) {
		t.Fatalf("writer balance %s, want 250", got)
	}
	var psStatus string
	pool.QueryRow(ctx, `SELECT premium_status FROM option_positions WHERE id=$1`, holdID).Scan(&psStatus)
	if psStatus != "SETTLED" {
		t.Fatalf("premium_status %s", psStatus)
	}
	// Replay-safe.
	settled, failed, err = svc.RunPremiumSweep(ctx)
	if settled != 0 || failed != 0 || err != nil {
		t.Fatalf("re-sweep settled=%d failed=%d err=%v", settled, failed, err)
	}
	if len(pub.subjects) == 0 {
		t.Fatalf("no balance events published")
	}
}

func TestPgxPremiumInsufficientQueuesMarginCall(t *testing.T) {
	pool := lifecycleFixture(t)
	ctx := context.Background()
	seedAccount(t, pool, 21, "10") // short of the 250 premium
	seedAccount(t, pool, 22, "0")
	mc := &fakeMarginCalls{}
	clk := &testClock{t: day(2026, 2, 24)}
	svc, _ := lifecycleSvc(t, pool, "1.1055",
		WithMarginCallQueuer(mc), WithOptionClock(clk.now))

	expiry := day(2026, 3, 6).Add(10 * time.Hour)
	seedOptionBook(t, pool, svc, 21, 22, "CALL", SettlementCash,
		"1.10", "10000", "250", expiry, day(2026, 2, 24))

	clk.set(day(2026, 2, 27)) // due day
	settled, failed, err := svc.RunPremiumSweep(ctx)
	if err != nil || failed != 1 || settled != 0 {
		t.Fatalf("sweep settled=%d failed=%d err=%v", settled, failed, err)
	}
	if len(mc.queued) != 1 {
		t.Fatalf("margin call not queued: %v", mc.queued)
	}
	var st, reason string
	pool.QueryRow(ctx, `SELECT status, failure_reason FROM option_premium_settlements`).Scan(&st, &reason)
	if st != "FAILED" || !strings.Contains(reason, "PREMIUM_INSUFFICIENT") {
		t.Fatalf("settlement status=%s reason=%s", st, reason)
	}
	var mcq bool
	pool.QueryRow(ctx, `SELECT margin_call_queued FROM option_premium_settlements`).Scan(&mcq)
	if !mcq {
		t.Fatalf("margin_call_queued flag not set")
	}
	// Balances untouched.
	if got := avail(t, pool, 21, "USD"); !got.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("holder balance mutated: %s", got)
	}
}

// ---------------------------------------------------------------------------
// Manual exercise — cutoff + cash delivery
// ---------------------------------------------------------------------------

func TestPgxManualExerciseCash(t *testing.T) {
	pool := lifecycleFixture(t)
	ctx := context.Background()
	seedAccount(t, pool, 31, "0")      // holder
	seedAccount(t, pool, 32, "100000") // writer — funds the intrinsic
	clk := &testClock{t: day(2026, 2, 20)}
	svc, _ := lifecycleSvc(t, pool, "1.1055", WithOptionClock(clk.now))

	expiry := day(2026, 3, 2).Add(10 * time.Hour) // Monday expiry day
	holdID, writeID := seedOptionBook(t, pool, svc, 31, 32, "CALL", SettlementCash,
		"1.10", "10000", "0", expiry, day(2026, 2, 20))
	// Advance to the expiry day, pre-cutoff → EUROPEAN is exercisable.
	clk.set(day(2026, 3, 2).Add(10 * time.Hour))

	rep, err := svc.ManualExercise(ctx, 31, holdID)
	if err != nil {
		t.Fatalf("exercise: %v", err)
	}
	if rep.Source != "MANUAL" || rep.Assignments != 1 {
		t.Fatalf("report %+v", rep)
	}
	if got := optStatus(t, pool, holdID); got != OptStatusExercised {
		t.Fatalf("holder status %s", got)
	}
	if got := optStatus(t, pool, writeID); got != OptStatusAssigned {
		t.Fatalf("writer status %s", got)
	}
	// Cash intrinsic = (mark − strike) × qty = 0.0055 × 10000 = 55 USD
	// moves writer → holder.
	if got := avail(t, pool, 31, "USD"); !got.Equal(decimal.RequireFromString("55")) {
		t.Fatalf("holder intrinsic %s, want 55", got)
	}
	if got := avail(t, pool, 32, "USD"); !got.Equal(decimal.RequireFromString("99945")) {
		t.Fatalf("writer intrinsic %s, want 99945", got)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM option_assignments`).Scan(&n)
	if n != 1 {
		t.Fatalf("assignments %d", n)
	}
	// Second exercise → coded terminal-state rejection.
	_, err = svc.ManualExercise(ctx, 31, holdID)
	assertCode(t, err, CodeOptionNotExercisable, "re-exercise")
}

func TestPgxManualExerciseAfterCutoffRejected(t *testing.T) {
	pool := lifecycleFixture(t)
	ctx := context.Background()
	seedAccount(t, pool, 41, "0")
	seedAccount(t, pool, 42, "100000")
	// Mutable clock — register at trade time, then jump past the
	// 15:00 UTC cutoff on the expiry day.
	clk := &testClock{t: day(2026, 2, 20)}
	ls, err := settlement.NewLedgerService(pool, nil, &lifecycleFakePub{})
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	svc, err := NewOptionService(pool, ls,
		fakeSpot{m: map[string]oracle.MarkView{
			"EURUSD": {Price: decimal.RequireFromString("1.12"), ValidAt: clk.now()},
		}},
		noopLocker{}, &lifecycleFakePub{}, WithOptionClock(clk.now))
	if err != nil {
		t.Fatalf("svc: %v", err)
	}
	expiry := day(2026, 3, 2).Add(10 * time.Hour) // expiry day = 2026-03-02
	holdID, _ := seedOptionBook(t, pool, svc, 41, 42, "CALL", SettlementCash,
		"1.10", "10000", "0", expiry, day(2026, 2, 20))
	clk.set(day(2026, 3, 2).Add(16 * time.Hour))
	_, err = svc.ManualExercise(ctx, 41, holdID)
	assertCode(t, err, CodeExerciseCutoffPassed, "post-cutoff exercise")
	if got := optStatus(t, pool, holdID); got != OptStatusOpen {
		t.Fatalf("post-cutoff exercise mutated status: %s", got)
	}
}

// fakeAuctionGuard is the §24 #247 liquidation-auction seam — canned
// ActiveAuctions answers for the exercise gate.
type fakeAuctionGuard struct {
	rows []risk.AuctionRow
	err  error
}

func (f *fakeAuctionGuard) ActiveAuctions(context.Context) ([]risk.AuctionRow, error) {
	return f.rows, f.err
}

func TestPgxManualExerciseAuctionGate(t *testing.T) {
	pool := lifecycleFixture(t)
	ctx := context.Background()
	seedAccount(t, pool, 61, "0")
	seedAccount(t, pool, 62, "100000")
	clk := &testClock{t: day(2026, 2, 20)}
	guard := &fakeAuctionGuard{}
	svc, _ := lifecycleSvc(t, pool, "1.1055", WithOptionClock(clk.now),
		WithExerciseAuctionGuard(guard))
	expiry := day(2026, 3, 2).Add(10 * time.Hour)
	holdID, _ := seedOptionBook(t, pool, svc, 61, 62, "CALL", SettlementCash,
		"1.10", "10000", "0", expiry, day(2026, 2, 20))
	clk.set(day(2026, 3, 2).Add(10 * time.Hour))

	// Auction on the option instrument → blocked; nothing mutates.
	guard.rows = []risk.AuctionRow{{ID: 9, InstrumentID: 701, Phase: "CALL"}}
	_, err := svc.ManualExercise(ctx, 61, holdID)
	assertCode(t, err, CodeExerciseAuctionBlocked, "auction on option instrument")
	if got := optStatus(t, pool, holdID); got != OptStatusOpen {
		t.Fatalf("blocked exercise mutated status: %s", got)
	}
	// Auction on the underlying → blocked (assignment delivers into it).
	guard.rows = []risk.AuctionRow{{ID: 10, InstrumentID: 702, Phase: "EXTEND"}}
	_, err = svc.ManualExercise(ctx, 61, holdID)
	assertCode(t, err, CodeExerciseAuctionBlocked, "auction on underlying")
	// Guard read failure → fail-closed 503.
	guard.rows = nil
	guard.err = errors.New("auction store down")
	_, err = svc.ManualExercise(ctx, 61, holdID)
	assertCode(t, err, CodeExerciseAuctionEvalFailed, "guard read failure")
	// Unrelated auction → exercise proceeds normally.
	guard.err = nil
	guard.rows = []risk.AuctionRow{{ID: 11, InstrumentID: 999, Phase: "CALL"}}
	rep, err := svc.ManualExercise(ctx, 61, holdID)
	if err != nil {
		t.Fatalf("unrelated auction must not block: %v", err)
	}
	if rep.Assignments != 1 {
		t.Fatalf("report %+v", rep)
	}
}

func svcLedger(t *testing.T, pool *pgxpool.Pool) *settlement.LedgerService {
	t.Helper()
	ls, err := settlement.NewLedgerService(pool, nil, &lifecycleFakePub{})
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	return ls
}

// ---------------------------------------------------------------------------
// Expiry sweep — ITM auto-exercise, OTM expiry, DNE, run idempotency
// ---------------------------------------------------------------------------

func TestPgxExpirySweep(t *testing.T) {
	pool := lifecycleFixture(t)
	ctx := context.Background()
	seedAccount(t, pool, 51, "0")      // ITM holder
	seedAccount(t, pool, 52, "0")      // OTM holder
	seedAccount(t, pool, 53, "0")      // DNE holder
	seedAccount(t, pool, 54, "100000") // writer
	clk := &testClock{t: day(2026, 2, 20)}
	// Mark 1.11 → CALL @1.10 is ~90bps ITM; PUT @1.10 is OTM.
	ls := svcLedger(t, pool)
	svc, err := NewOptionService(pool, ls,
		fakeSpot{m: map[string]oracle.MarkView{
			"EURUSD": {Price: decimal.RequireFromString("1.11"), ValidAt: clk.now()},
		}},
		noopLocker{}, &lifecycleFakePub{}, WithOptionClock(clk.now))
	if err != nil {
		t.Fatalf("svc: %v", err)
	}
	expiry := day(2026, 3, 2).Add(10 * time.Hour) // expires on the run day
	itmID, w1 := seedOptionBook(t, pool, svc, 51, 54, "CALL", SettlementCash,
		"1.10", "10000", "0", expiry, day(2026, 2, 20))
	otmID, _ := seedOptionBookIDs(t, pool, svc, 52, "PUT", SettlementCash,
		"1.10", "10000", "0", expiry, day(2026, 2, 20), 710)
	dneID, _ := seedOptionBookIDs(t, pool, svc, 53, "CALL", SettlementCash,
		"1.10", "10000", "0", expiry, day(2026, 2, 20), 720)
	// do_not_exercise lands pre-cutoff — the instruction path itself is
	// covered by DoNotExercise; here the flag is stamped directly.
	if _, err := pool.Exec(ctx,
		`UPDATE option_positions SET do_not_exercise=TRUE WHERE id=$1`, dneID); err != nil {
		t.Fatalf("dne flag: %v", err)
	}

	clk.set(day(2026, 3, 2).Add(16 * time.Hour)) // 15:00 batch time
	rep, err := svc.RunExpiryDay(ctx, day(2026, 3, 2))
	if err != nil {
		t.Fatalf("expiry run: %v", err)
	}
	if rep.AutoExercised != 1 || rep.Expired != 2 {
		t.Fatalf("report %+v", rep)
	}
	if got := optStatus(t, pool, itmID); got != OptStatusExercised {
		t.Fatalf("ITM holder %s", got)
	}
	if got := optStatus(t, pool, otmID); got != OptStatusExpired {
		t.Fatalf("OTM holder %s", got)
	}
	if got := optStatus(t, pool, dneID); got != OptStatusExpired {
		t.Fatalf("DNE holder %s", got)
	}
	// All writers on the instrument pool eventually expire/assign.
	if got := optStatus(t, pool, w1); got != OptStatusAssigned {
		t.Fatalf("assigned writer %s", got)
	}

	// Idempotent: second run for the same day replays the stored report.
	rep2, err := svc.RunExpiryDay(ctx, day(2026, 3, 2))
	if err != nil || !rep2.Replayed || rep2.RunID != rep.RunID {
		t.Fatalf("replay %+v err=%v", rep2, err)
	}
}

// seedOptionBookIDs is seedOptionBook with an explicit instrument id so
// multiple option series coexist in one fixture.
func seedOptionBookIDs(t *testing.T, pool *pgxpool.Pool, svc *OptionService,
	holderAcct int64, optType, settlementMode string,
	strike, qty, premium string, expiry, tradeDate time.Time, instrID int64) (holderOptID, writerPosID int64) {
	t.Helper()
	ctx := context.Background()
	sym := fmt.Sprintf("OPT%d", instrID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO instruments (id, symbol, instrument_type, base_currency, quote_currency, max_leverage)
		VALUES ($1,$2,'OPTION','EUR','USD',30) ON CONFLICT (id) DO NOTHING`, instrID, sym); err != nil {
		t.Fatalf("instrument %d: %v", instrID, err)
	}
	var pos int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO positions (account_id, instrument_id, side, quantity, entry_price)
		VALUES ($1,$2,'LONG',$3,$4) RETURNING id`,
		holderAcct, instrID, qty, strike).Scan(&pos); err != nil {
		t.Fatalf("position: %v", err)
	}
	id, err := svc.RegisterOptionPosition(ctx, OptionRegistration{
		PositionID: pos, AccountID: holderAcct,
		WriterAccountID: 0, UnderlyingInstrumentID: 702,
		OptionType: optType, ExerciseStyle: "EUROPEAN",
		Settlement: settlementMode,
		Strike:     decimal.RequireFromString(strike),
		Quantity:   decimal.RequireFromString(qty),
		ExpiryAt:   expiry, Premium: decimal.RequireFromString(premium),
		PremiumCurrency: "USD", TradeDate: tradeDate,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return id, 0
}

func TestPgxExpirySweepStaleMarkFailsClosed(t *testing.T) {
	pool := lifecycleFixture(t)
	ctx := context.Background()
	seedAccount(t, pool, 61, "0")
	seedAccount(t, pool, 62, "100000")
	clk := &testClock{t: day(2026, 2, 20)}
	ls := svcLedger(t, pool)
	svc, err := NewOptionService(pool, ls,
		fakeSpot{m: map[string]oracle.MarkView{
			"EURUSD": {Price: decimal.RequireFromString("1.11"), Stale: true},
		}},
		noopLocker{}, &lifecycleFakePub{}, WithOptionClock(clk.now))
	if err != nil {
		t.Fatalf("svc: %v", err)
	}
	expiry := day(2026, 3, 2).Add(10 * time.Hour)
	optID, _ := seedOptionBook(t, pool, svc, 61, 62, "CALL", SettlementCash,
		"1.10", "10000", "0", expiry, day(2026, 2, 20))
	clk.set(day(2026, 3, 2).Add(16 * time.Hour))
	rep, err := svc.RunExpiryDay(ctx, day(2026, 3, 2))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rep.Errors) == 0 {
		t.Fatalf("stale mark must surface an error")
	}
	if got := optStatus(t, pool, optID); got != OptStatusOpen {
		t.Fatalf("stale mark exercised/expired the option: %s", got)
	}
}
