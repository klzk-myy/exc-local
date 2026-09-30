// isolated_margin_test.go — Phase-19 Task 19.3.27 coverage: pure level/
// boundary math, allocation lifecycle (insufficient/wrong-mode guards),
// auto-replenish averting liquidation, deficit → position-scoped
// dispatch, no cross-contamination, and the gated PG store leg.
package risk

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type isoBal struct{ avail, locked decimal.Decimal }

type isoPos struct {
	allocated decimal.Decimal
	auto      bool
	accountID int64
	boundary  decimal.Decimal
	leg       *MarginPosition // returned by PositionLeg
}

// isoStoreFake implements IsolatedMarginStore in memory.
type isoStoreFake struct {
	mu    sync.Mutex
	modes map[int64]MarginMode
	bases map[int64]string
	bals  map[int64]map[string]isoBal
	poss  map[int64]*isoPos

	allocErr error
	replErr  error
}

func newIsoStoreFake() *isoStoreFake {
	return &isoStoreFake{
		modes: map[int64]MarginMode{},
		bases: map[int64]string{},
		bals:  map[int64]map[string]isoBal{},
		poss:  map[int64]*isoPos{},
	}
}

func (f *isoStoreFake) PositionLeg(_ context.Context, id int64) (*MarginPosition, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.poss[id]
	if !ok || p.leg == nil {
		return nil, 0, nil
	}
	leg := *p.leg
	leg.IsolatedAllocated = p.allocated
	leg.AutoReplenish = p.auto
	return &leg, p.accountID, nil
}
func (f *isoStoreFake) MarginModeFor(_ context.Context, id int64) (MarginMode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.modes[id]
	if !ok {
		return "", fmt.Errorf("fake: account %d not found", id)
	}
	return m, nil
}
func (f *isoStoreFake) BaseCurrency(_ context.Context, id int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bases[id], nil
}
func (f *isoStoreFake) Available(_ context.Context, id int64, ccy string) (decimal.Decimal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bals[id][ccy].avail, nil
}
func (f *isoStoreFake) Allocate(_ context.Context, accountID, positionID int64,
	ccy string, amount decimal.Decimal, auto bool) error {

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.allocErr != nil {
		return f.allocErr
	}
	b := f.bals[accountID][ccy]
	if b.avail.LessThan(amount) {
		return ErrInsufficientBalance
	}
	p, ok := f.poss[positionID]
	if !ok {
		return ErrPositionNotOpen
	}
	b.avail = b.avail.Sub(amount)
	b.locked = b.locked.Add(amount)
	f.bals[accountID][ccy] = b
	p.allocated = p.allocated.Add(amount)
	p.auto = auto
	return nil
}
func (f *isoStoreFake) Replenish(_ context.Context, accountID, positionID int64,
	ccy string, amount decimal.Decimal) (decimal.Decimal, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replErr != nil {
		return decimal.Zero, f.replErr
	}
	b := f.bals[accountID][ccy]
	moved := amount
	if b.avail.LessThan(moved) {
		moved = b.avail
	}
	if !moved.IsPositive() {
		return decimal.Zero, nil
	}
	p, ok := f.poss[positionID]
	if !ok {
		return decimal.Zero, ErrPositionNotOpen
	}
	b.avail = b.avail.Sub(moved)
	b.locked = b.locked.Add(moved)
	f.bals[accountID][ccy] = b
	p.allocated = p.allocated.Add(moved)
	return moved, nil
}
func (f *isoStoreFake) SetLiquidationBoundary(_ context.Context, positionID int64,
	price decimal.Decimal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.poss[positionID]
	if !ok {
		return ErrPositionNotOpen
	}
	p.boundary = price
	return nil
}

// rateSourceFake implements IsolatedRateSource.
type rateSourceFake struct{ rates map[string]decimal.Decimal }

func (r rateSourceFake) RateToUSD(_ context.Context, ccy string) (decimal.Decimal, bool) {
	v, ok := r.rates[ccy]
	return v, ok
}

// ---------------------------------------------------------------------------
// Pure math
// ---------------------------------------------------------------------------

func TestIsolatedLevelPct(t *testing.T) {
	l := IsolatedLevelPct(d("1000"), d("-600"), d("1000"))
	if l == nil || !l.Equal(d("40")) {
		t.Fatalf("level %v want 40", l)
	}
	l = IsolatedLevelPct(d("500"), d("600"), d("1000"))
	if !l.Equal(d("110")) {
		t.Fatalf("level %v want 110", l)
	}
	if IsolatedLevelPct(d("1"), d("1"), decimal.Zero) != nil {
		t.Fatal("non-positive mmr must return nil")
	}
}

func TestIsolatedTopUpUSD(t *testing.T) {
	if !IsolatedTopUpUSD(d("400"), d("0"), d("1000")).Equal(d("600")) {
		t.Fatal("topup 600")
	}
	if !IsolatedTopUpUSD(d("1000"), d("100"), d("1000")).IsZero() {
		t.Fatal("surplus must top up zero")
	}
}

func TestIsolatedBoundaryMark(t *testing.T) {
	// LONG 10000 @ 1.10, reqQ 1000, allocated 1000 → breach at upnl −500
	// → p* = 1.10 − 0.05 = 1.05.
	pos := MarginPosition{Side: "LONG", Quantity: d("10000"), EntryPrice: d("1.10"),
		MarginUsed: d("1000"), IsolatedAllocated: d("1000")}
	p, ok := IsolatedBoundaryMark(pos, d("1.10"), decimal.One, d("50"))
	if !ok || !p.Equal(d("1.05")) {
		t.Fatalf("long boundary %s ok=%v want 1.05", p, ok)
	}
	// SHORT → breach on the way up: 1.15.
	pos.Side = "SHORT"
	p, ok = IsolatedBoundaryMark(pos, d("1.10"), decimal.One, d("50"))
	if !ok || !p.Equal(d("1.15")) {
		t.Fatalf("short boundary %s ok=%v want 1.15", p, ok)
	}
	// Margin_used absent → qty×mark/leverage fallback.
	pos.MarginUsed = decimal.Zero
	pos.MaxLeverage = 10
	p, ok = IsolatedBoundaryMark(pos, d("1.10"), decimal.One, d("50"))
	if !ok || !p.IsPositive() {
		t.Fatalf("fallback-req boundary %s ok=%v", p, ok)
	}
}

// ---------------------------------------------------------------------------
// Allocate lifecycle
// ---------------------------------------------------------------------------

func TestAllocateInitialMarginLocksAndBoundaries(t *testing.T) {
	ctx := context.Background()
	st := newIsoStoreFake()
	st.modes[1] = ModeIsolated
	st.bases[1] = "USD"
	st.bals[1] = map[string]isoBal{"USD": {avail: d("5000")}}
	leg := &MarginPosition{ID: 10, Symbol: "EUR/USD", Side: "LONG",
		Quantity: d("10000"), EntryPrice: d("1.10"), MarginUsed: d("1000"),
		QuoteCurrency: "USD", MaxLeverage: 30}
	st.poss[10] = &isoPos{accountID: 1, leg: leg}
	st.poss[10].leg.IsolatedAllocated = decimal.Zero

	dspy := &dispatcherSpy{enq: true}
	svc, err := NewIsolatedMarginService(IsolatedMarginDeps{
		Store: st, Dispatcher: dspy,
		Rates: rateSourceFake{rates: map[string]decimal.Decimal{"USD": d("1")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.AllocateInitialMargin(ctx, IsolatedAllocateRequest{
		AccountID: 1, PositionID: 10, Currency: "USD",
		Amount: d("1000"), AutoReplenish: true}); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	b := st.bals[1]["USD"]
	if !b.avail.Equal(d("4000")) || !b.locked.Equal(d("1000")) {
		t.Fatalf("balance move wrong: avail %s locked %s", b.avail, b.locked)
	}
	if !st.poss[10].allocated.Equal(d("1000")) || !st.poss[10].auto {
		t.Fatalf("allocated %s auto %v", st.poss[10].allocated, st.poss[10].auto)
	}
	// Boundary: LONG 10000@1.10 req 1000 alloc 1000 → 1.05.
	if !st.poss[10].boundary.Equal(d("1.05")) {
		t.Fatalf("boundary %s want 1.05", st.poss[10].boundary)
	}
}

func TestAllocateInitialMarginGuards(t *testing.T) {
	ctx := context.Background()
	st := newIsoStoreFake()
	st.modes[1] = ModeIsolated
	st.modes[2] = ModeCross
	st.bases[1], st.bases[2] = "USD", "USD"
	st.bals[1] = map[string]isoBal{"USD": {avail: d("500")}}
	st.bals[2] = map[string]isoBal{"USD": {avail: d("5000")}}
	st.poss[10] = &isoPos{accountID: 1}
	st.poss[11] = &isoPos{accountID: 2}
	dspy := &dispatcherSpy{enq: true}
	svc, err := NewIsolatedMarginService(IsolatedMarginDeps{Store: st, Dispatcher: dspy})
	if err != nil {
		t.Fatal(err)
	}
	// Insufficient available → MARGIN_INSUFFICIENT.
	err = svc.AllocateInitialMargin(ctx, IsolatedAllocateRequest{
		AccountID: 1, PositionID: 10, Currency: "USD", Amount: d("1000")})
	if excerrors.CodeOf(err) != CodeMarginInsufficient {
		t.Fatalf("insufficient → code %s", excerrors.CodeOf(err))
	}
	// Non-ISOLATED account → INVALID_REQUEST.
	err = svc.AllocateInitialMargin(ctx, IsolatedAllocateRequest{
		AccountID: 2, PositionID: 11, Currency: "USD", Amount: d("100")})
	if excerrors.CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("wrong mode → code %s", excerrors.CodeOf(err))
	}
	// Nothing moved on either rejection.
	if !st.bals[1]["USD"].avail.Equal(d("500")) || !st.bals[2]["USD"].avail.Equal(d("5000")) {
		t.Fatal("rejected allocate must not move funds")
	}
}

// ---------------------------------------------------------------------------
// Deficit handling — replenish averts, deficit dispatches position-scoped
// ---------------------------------------------------------------------------

// isoLegs builds a minimal accountLegs for CheckAccount tests.
func isoLegs(acct int64, avail string, pos ...MarginPosition) *accountLegs {
	return &accountLegs{
		acct:      &MarginAccount{AccountID: acct, Mode: ModeIsolated, Status: "NORMAL"},
		category:  "RETAIL",
		mode:      ModeIsolated,
		baseCcy:   "USD",
		balances:  []BalanceAmount{{Currency: "USD", Available: d(avail)}},
		positions: pos,
	}
}

func isoSnap(acct int64, evals ...PositionMarginEval) *MarginSnapshot {
	return &MarginSnapshot{AccountID: acct, Mode: ModeIsolated, Status: "NORMAL",
		Positions: evals, Ts: time.Now()}
}

func usdRate(ccy string) (decimal.Decimal, bool) {
	if ccy == "USD" {
		return decimal.One, true
	}
	return decimal.Zero, false
}

func TestCheckAccountReplenishAvertsLiquidation(t *testing.T) {
	ctx := context.Background()
	st := newIsoStoreFake()
	st.bals[9] = map[string]isoBal{"USD": {avail: d("5000")}}
	leg := &MarginPosition{ID: 101, Symbol: "EUR/USD", Side: "LONG",
		Quantity: d("10000"), EntryPrice: d("1.10"), MarginUsed: d("1000"),
		QuoteCurrency: "USD", MaxLeverage: 30}
	st.poss[101] = &isoPos{allocated: d("1000"), auto: true, accountID: 9, leg: leg}
	dspy := &dispatcherSpy{enq: true}
	svc, err := NewIsolatedMarginService(IsolatedMarginDeps{Store: st, Dispatcher: dspy})
	if err != nil {
		t.Fatal(err)
	}
	pos := *leg
	pos.IsolatedAllocated = d("1000")
	pos.AutoReplenish = true
	legs := isoLegs(9, "5000", pos)
	// upnl −600 → level (1000−600)/1000 = 40% → breach; need 600 to restore.
	snap := isoSnap(9, PositionMarginEval{PositionID: 101, MarginUSD: "1000", UnrealizedUSD: "-600"})
	if err := svc.CheckAccount(ctx, legs, snap, usdRate); err != nil {
		t.Fatalf("check: %v", err)
	}
	if dspy.count() != 0 {
		t.Fatal("replenish must avert dispatch")
	}
	b := st.bals[9]["USD"]
	if !b.avail.Equal(d("4400")) || !b.locked.Equal(d("600")) {
		t.Fatalf("replenish move: avail %s locked %s", b.avail, b.locked)
	}
	if !st.poss[101].allocated.Equal(d("1600")) {
		t.Fatalf("allocated %s want 1600", st.poss[101].allocated)
	}
	// Post-replenish level = (1600−600)/1000 = 100% — restored.
	l := IsolatedLevelPct(st.poss[101].allocated, d("-600"), d("1000"))
	if !l.Equal(d("100")) {
		t.Fatalf("restored level %s want 100", l)
	}
}

func TestCheckAccountDeficitDispatchesPositionScoped(t *testing.T) {
	ctx := context.Background()
	st := newIsoStoreFake()
	st.bals[9] = map[string]isoBal{"USD": {avail: d("5000")}}
	pos := MarginPosition{ID: 101, Symbol: "EUR/USD", Side: "LONG",
		Quantity: d("10000"), EntryPrice: d("1.10"), MarginUsed: d("1000"),
		QuoteCurrency: "USD", MaxLeverage: 30,
		IsolatedAllocated: d("1000"), AutoReplenish: false}
	// Second, healthy position — must remain untouched.
	pos2 := MarginPosition{ID: 102, Symbol: "GBP/USD", Side: "LONG",
		Quantity: d("5000"), EntryPrice: d("1.25"), MarginUsed: d("500"),
		QuoteCurrency: "USD", MaxLeverage: 30,
		IsolatedAllocated: d("500"), AutoReplenish: false}
	st.poss[101] = &isoPos{allocated: d("1000"), accountID: 9}
	st.poss[102] = &isoPos{allocated: d("500"), accountID: 9}
	dspy := &dispatcherSpy{enq: true}
	svc, err := NewIsolatedMarginService(IsolatedMarginDeps{Store: st, Dispatcher: dspy})
	if err != nil {
		t.Fatal(err)
	}
	legs := isoLegs(9, "5000", pos, pos2)
	snap := isoSnap(9,
		PositionMarginEval{PositionID: 101, MarginUSD: "1000", UnrealizedUSD: "-600"}, // 40%
		PositionMarginEval{PositionID: 102, MarginUSD: "500", UnrealizedUSD: "0"})     // 100%
	if err := svc.CheckAccount(ctx, legs, snap, usdRate); err != nil {
		t.Fatal(err)
	}
	if dspy.count() != 1 {
		t.Fatalf("want 1 dispatch, got %d", dspy.count())
	}
	job := dspy.last()
	if job.PositionID != 101 || job.Reason != LiquidationReasonIsolatedDeficit ||
		job.AccountID != 9 {
		t.Fatalf("job %+v", job)
	}
	if job.MarginLevelPct != "40" {
		t.Fatalf("job level %s want 40", job.MarginLevelPct)
	}
	// No cross-contamination: balance + other position untouched.
	if !st.bals[9]["USD"].avail.Equal(d("5000")) {
		t.Fatal("available balance must be untouched when auto-replenish off")
	}
	if !st.poss[102].allocated.Equal(d("500")) {
		t.Fatal("healthy position allocation must be untouched")
	}
}

func TestCheckAccountInsufficientReplenishStillLiquidates(t *testing.T) {
	ctx := context.Background()
	st := newIsoStoreFake()
	st.bals[9] = map[string]isoBal{"USD": {avail: d("100")}} // need 600, have 100
	pos := MarginPosition{ID: 101, Symbol: "EUR/USD", Side: "LONG",
		Quantity: d("10000"), EntryPrice: d("1.10"), MarginUsed: d("1000"),
		QuoteCurrency: "USD", MaxLeverage: 30,
		IsolatedAllocated: d("1000"), AutoReplenish: true}
	st.poss[101] = &isoPos{allocated: d("1000"), auto: true, accountID: 9}
	dspy := &dispatcherSpy{enq: true}
	svc, err := NewIsolatedMarginService(IsolatedMarginDeps{Store: st, Dispatcher: dspy})
	if err != nil {
		t.Fatal(err)
	}
	legs := isoLegs(9, "100", pos)
	snap := isoSnap(9, PositionMarginEval{PositionID: 101, MarginUSD: "1000", UnrealizedUSD: "-600"})
	if err := svc.CheckAccount(ctx, legs, snap, usdRate); err != nil {
		t.Fatal(err)
	}
	if dspy.count() != 1 {
		t.Fatalf("partial replenish cannot avert liquidation, got %d dispatches", dspy.count())
	}
	if dspy.last().PositionID != 101 {
		t.Fatal("position-scoped job expected")
	}
	// The partial 100 still moved into the allocation (returns via close).
	if !st.poss[101].allocated.Equal(d("1100")) || !st.bals[9]["USD"].avail.IsZero() {
		t.Fatalf("partial move: alloc %s avail %s",
			st.poss[101].allocated, st.bals[9]["USD"].avail)
	}
}

func TestCheckAccountHealthyPositionNoAction(t *testing.T) {
	ctx := context.Background()
	st := newIsoStoreFake()
	st.bals[9] = map[string]isoBal{"USD": {avail: d("5000")}}
	pos := MarginPosition{ID: 101, Symbol: "EUR/USD", Side: "LONG",
		Quantity: d("10000"), EntryPrice: d("1.10"), MarginUsed: d("1000"),
		QuoteCurrency: "USD", MaxLeverage: 30,
		IsolatedAllocated: d("1000"), AutoReplenish: true}
	st.poss[101] = &isoPos{allocated: d("1000"), auto: true, accountID: 9}
	dspy := &dispatcherSpy{enq: true}
	svc, err := NewIsolatedMarginService(IsolatedMarginDeps{Store: st, Dispatcher: dspy})
	if err != nil {
		t.Fatal(err)
	}
	legs := isoLegs(9, "5000", pos)
	snap := isoSnap(9, PositionMarginEval{PositionID: 101, MarginUSD: "1000", UnrealizedUSD: "0"})
	if err := svc.CheckAccount(ctx, legs, snap, usdRate); err != nil {
		t.Fatal(err)
	}
	if dspy.count() != 0 {
		t.Fatal("healthy position must not dispatch")
	}
	if !st.bals[9]["USD"].avail.Equal(d("5000")) {
		t.Fatal("no replenish on a healthy level")
	}
}

// ---------------------------------------------------------------------------
// Gated PG leg — PgIsolatedMarginStore on the scratch-schema pattern
// ---------------------------------------------------------------------------

// isoStoreFixture builds the throwaway schema with the migration subset
// the isolated store touches (balances + positions.106 + instruments +
// accounts.base_currency inline ALTER, mirroring liqStoreFixture).
func isoStoreFixture(t *testing.T) (*PgIsolatedMarginStore, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("EXC_PG_TEST not set")
	}
	dsn := liqStoreDSN()
	schema := fmt.Sprintf("isostore_itest_%d", time.Now().UnixNano())
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
		"004_create_balances.up.sql",
		"013_create_margin_accounts.up.sql",
		"014_create_positions.up.sql",
		"042_client_categorization.up.sql",
		"106_positions_isolated_margin.up.sql",
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

	if _, err := pool.Exec(ctx,
		`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS base_currency VARCHAR(3) NOT NULL DEFAULT 'USD'`); err != nil {
		t.Fatalf("base_currency: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`CREATE UNIQUE INDEX uq_margin_accounts_account_id ON margin_accounts (account_id)`); err != nil {
		t.Fatalf("uq index: %v", err)
	}
	st, err := NewPgIsolatedMarginStore(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return st, pool
}

func TestPgIsolatedMarginStoreLifecycle(t *testing.T) {
	st, pool := isoStoreFixture(t)
	ctx := context.Background()
	acct := liqSeedAccount(t, pool, "RETAIL")
	inst := liqSeedInstrument(t, pool)
	liqSeedMarginAccount(t, pool, acct, "ISOLATED", "")
	posID := liqSeedPosition(t, pool, acct, inst, "LONG", "10000", "1.10",
		liqStr("1.10"), nil, "0", "1000")
	if _, err := pool.Exec(ctx,
		`INSERT INTO balances (account_id, currency, available, locked)
		 VALUES ($1,'USD',5000,0)`, acct); err != nil {
		t.Fatalf("balance seed: %v", err)
	}

	// Mode + base + available reads.
	if m, err := st.MarginModeFor(ctx, acct); err != nil || m != ModeIsolated {
		t.Fatalf("mode %q %v", m, err)
	}
	if c, err := st.BaseCurrency(ctx, acct); err != nil || c != "USD" {
		t.Fatalf("base %q %v", c, err)
	}
	if a, err := st.Available(ctx, acct, "USD"); err != nil || !a.Equal(d("5000")) {
		t.Fatalf("avail %s %v", a, err)
	}

	// Allocate beyond available → ErrInsufficientBalance.
	if err := st.Allocate(ctx, acct, posID, "USD", d("9000"), false); err != ErrInsufficientBalance {
		t.Fatalf("insufficient allocate: %v", err)
	}
	// Allocate 1000 with auto flag.
	if err := st.Allocate(ctx, acct, posID, "USD", d("1000"), true); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	var avail, locked, alloc string
	var auto bool
	if err := pool.QueryRow(ctx, `
		SELECT available::text, locked::text FROM balances
		 WHERE account_id=$1 AND currency='USD'`, acct).Scan(&avail, &locked); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT isolated_margin_allocated::text, auto_margin_replenish
		 FROM positions WHERE id=$1`, posID).Scan(&alloc, &auto); err != nil {
		t.Fatal(err)
	}
	if !d(avail).Equal(d("4000")) || !d(locked).Equal(d("1000")) ||
		!d(alloc).Equal(d("1000")) || !auto {
		t.Fatalf("post-allocate: avail=%s locked=%s alloc=%s auto=%v",
			avail, locked, alloc, auto)
	}
	// Replenish 700 (avail 4000 covers) → allocated 1700, avail 3300.
	moved, err := st.Replenish(ctx, acct, posID, "USD", d("700"))
	if err != nil || !moved.Equal(d("700")) {
		t.Fatalf("replenish moved %s err %v", moved, err)
	}
	// Replenish beyond remaining avail → moves only what's there.
	moved, err = st.Replenish(ctx, acct, posID, "USD", d("99999"))
	if err != nil || !moved.Equal(d("3300")) {
		t.Fatalf("capped replenish moved %s err %v", moved, err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT isolated_margin_allocated::text FROM positions WHERE id=$1`,
		posID).Scan(&alloc); err != nil {
		t.Fatal(err)
	}
	if !d(alloc).Equal(d("5000")) {
		t.Fatalf("allocated %s want 5000", alloc)
	}
	// Boundary write + PositionLeg read-back.
	if err := st.SetLiquidationBoundary(ctx, posID, d("1.05")); err != nil {
		t.Fatalf("boundary: %v", err)
	}
	leg, owner, err := st.PositionLeg(ctx, posID)
	if err != nil || leg == nil {
		t.Fatalf("leg %v %+v", err, leg)
	}
	if owner != acct || leg.Symbol != "EUR/USD" || !leg.AutoReplenish ||
		!leg.IsolatedAllocated.Equal(d("5000")) {
		t.Fatalf("leg %+v owner %d", leg, owner)
	}
	var bp string
	if err := pool.QueryRow(ctx,
		`SELECT liquidation_price::text FROM positions WHERE id=$1`, posID).Scan(&bp); err != nil {
		t.Fatal(err)
	}
	if !d(bp).Equal(d("1.05")) {
		t.Fatalf("boundary stored %s", bp)
	}
	// Flat position → ErrPositionNotOpen on allocate (refund available
	// first so the balance check cannot mask the position check).
	if _, err := pool.Exec(ctx, `UPDATE positions SET quantity=0 WHERE id=$1`, posID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE balances SET available=100 WHERE account_id=$1 AND currency='USD'`, acct); err != nil {
		t.Fatal(err)
	}
	if err := st.Allocate(ctx, acct, posID, "USD", d("1"), false); err != ErrPositionNotOpen {
		t.Fatalf("closed-position allocate: %v", err)
	}
}
