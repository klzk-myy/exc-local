// pb_credit_test.go — unit + gated integration coverage for the
// prime-broker NOP/DSL credit gate (pb_credit.go; Phase-19 Task 19.3.7;
// spec §5.22/§13.7).
//
// Ungated legs cover scope selection, reservation acquire/release
// semantics over a fake store, breach→code mapping, utilization/alert
// math, USD conversion and the fail-closed seams (nil store, store
// errors). Gated legs exercise PgPBCreditStore against the real
// migration-037/232 schema plus the Redis utilization mirror and the
// 90% alert dedupe:
//
//	EXC_PG_TEST=1    go test ./internal/risk/ -run 'TestPgPBCredit' -v
//	EXC_REDIS_TEST=1 go test ./internal/risk/ -run 'TestRedisPBCredit' -v
package risk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/position"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fake store — PBCreditStore over captured calls
// ---------------------------------------------------------------------------

type pbStoreFake struct {
	mu           sync.Mutex
	limits       []PBCreditLimit
	limitsErr    error
	reserveErr   error
	reserveCalls [][]PBDebit
	releaseRows  []PBReservation
	releaseErr   error
	releaseCalls []int64
	consumeErr   error
	consumeCalls []int64
	reservations []PBReservation
	resErr       error
	adjustErr    error
	adjustCalls  []decimal.Decimal
	terminalN    int64
	terminalErr  error
	exposure     []PBInstrumentExposure
	expErr       error
	syncCalls    []pbSyncCall
	syncErr      error
	resetN       int64
	resetErr     error
}

type pbSyncCall struct {
	clientID  int64
	globalUSD decimal.Decimal
	pairUSD   map[string]decimal.Decimal
}

func (f *pbStoreFake) LimitsFor(context.Context, int64) ([]PBCreditLimit, error) {
	return f.limits, f.limitsErr
}
func (f *pbStoreFake) ReserveTx(_ context.Context, orderID, clientID int64,
	debits []PBDebit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserveCalls = append(f.reserveCalls, debits)
	return f.reserveErr
}
func (f *pbStoreFake) ReleaseTx(_ context.Context, orderID int64) ([]PBReservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls = append(f.releaseCalls, orderID)
	return f.releaseRows, f.releaseErr
}
func (f *pbStoreFake) ConsumeTx(_ context.Context, orderID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.consumeCalls = append(f.consumeCalls, orderID)
	return f.consumeErr
}
func (f *pbStoreFake) AdjustTx(_ context.Context, _ int64, deltaUSD decimal.Decimal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adjustCalls = append(f.adjustCalls, deltaUSD)
	return f.adjustErr
}
func (f *pbStoreFake) ReleaseTerminalTx(context.Context) (int64, error) {
	return f.terminalN, f.terminalErr
}
func (f *pbStoreFake) PositionExposure(context.Context, int64) ([]PBInstrumentExposure, error) {
	return f.exposure, f.expErr
}
func (f *pbStoreFake) SyncNOPTx(_ context.Context, clientID int64,
	globalUSD decimal.Decimal, pairUSD map[string]decimal.Decimal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncCalls = append(f.syncCalls, pbSyncCall{clientID, globalUSD, pairUSD})
	return f.syncErr
}
func (f *pbStoreFake) ResetDailySettled(context.Context) (int64, error) {
	return f.resetN, f.resetErr
}
func (f *pbStoreFake) UpsertLimit(_ context.Context, l PBCreditLimit) (PBCreditLimit, error) {
	return l, nil
}
func (f *pbStoreFake) DeleteLimit(context.Context, int64) error { return nil }
func (f *pbStoreFake) ListLimits(context.Context, int64) ([]PBCreditLimit, error) {
	return f.limits, nil
}
func (f *pbStoreFake) ReservationsFor(context.Context, int64) ([]PBReservation, error) {
	return f.reservations, f.resErr
}

// pbRateFake is a static FXRateProvider for non-USD quote conversions.
type pbRateFake map[string]decimal.Decimal

func (f pbRateFake) MidRate(_ context.Context, p position.Pair) (decimal.Decimal, error) {
	if v, ok := f[p.String()]; ok {
		return v, nil
	}
	return decimal.Zero, position.ErrPairNotFound
}

// ---------------------------------------------------------------------------
// normPair / Scope helpers
// ---------------------------------------------------------------------------

func TestPBNormPair(t *testing.T) {
	for in, want := range map[string]string{
		"EUR/USD": "EURUSD", "eur/usd": "EURUSD", "EURUSD": "EURUSD",
		"usd-jpy": "USDJPY", "GBP USD": "GBPUSD",
	} {
		if got := normPair(in); got != want {
			t.Fatalf("normPair(%q)=%q, want %q", in, got, want)
		}
	}
	// Scope passes the stored pair through verbatim — comparisons run
	// through normPair, Scope is the display label.
	pair := "eurusd"
	l := PBCreditLimit{CurrencyPair: &pair}
	if l.Scope() != "eurusd" {
		t.Fatalf("scope: %s, want stored form passthrough", l.Scope())
	}
	if (PBCreditLimit{}).Scope() != "GLOBAL" {
		t.Fatal("nil pair must scope GLOBAL")
	}
}

// ---------------------------------------------------------------------------
// ReserveHeadroom — scope selection + breach mapping
// ---------------------------------------------------------------------------

func pbLimits() []PBCreditLimit {
	nop := d("1000000")
	dsl := d("2000000")
	pair := "EURUSD"
	pairNOP := d("500000")
	return []PBCreditLimit{
		{ID: 1, PrimeBrokerID: 10, ClientAccountID: 7,
			NOPLimit: &nop, DSLLimit: &dsl}, // GLOBAL
		{ID: 2, PrimeBrokerID: 10, ClientAccountID: 7,
			CurrencyPair: &pair, NOPLimit: &pairNOP}, // pair scope
		{ID: 3, PrimeBrokerID: 10, ClientAccountID: 7,
			CurrencyPair: strp("USDJPY"), NOPLimit: &pairNOP}, // other pair
	}
}

func TestPBReserveHeadroomNonClient(t *testing.T) {
	f := &pbStoreFake{} // no limit rows
	svc := NewPBCreditService(f, nil, nil)
	if err := svc.ReserveHeadroom(context.Background(), 100, 7,
		"EURUSD", "USD", d("50000")); err != nil {
		t.Fatalf("non-PB account must admit: %v", err)
	}
	if len(f.reserveCalls) != 0 {
		t.Fatalf("no reservation must be taken, got %v", f.reserveCalls)
	}
	// A nil store is a misconfiguration — fail closed.
	svc = NewPBCreditService(nil, nil, nil)
	requireCode(t, svc.ReserveHeadroom(context.Background(), 100, 7,
		"EURUSD", "USD", d("1")), CodeRiskLimitsInternal)
	// Store error rejects.
	f = &pbStoreFake{limitsErr: fmt.Errorf("pg down")}
	svc = NewPBCreditService(f, nil, nil)
	requireCode(t, svc.ReserveHeadroom(context.Background(), 100, 7,
		"EURUSD", "USD", d("1")), CodeRiskLimitsInternal)
}

func TestPBReserveHeadroomScopeSelection(t *testing.T) {
	f := &pbStoreFake{limits: pbLimits()}
	svc := NewPBCreditService(f, nil, nil)
	err := svc.ReserveHeadroom(context.Background(), 100, 7,
		"eur/usd", "USD", d("50000"))
	if err != nil {
		t.Fatal(err)
	}
	// GLOBAL + matching EURUSD rows debit; the USDJPY row stays out.
	if len(f.reserveCalls) != 1 || len(f.reserveCalls[0]) != 2 {
		t.Fatalf("debits: %+v", f.reserveCalls)
	}
	got := map[int64]decimal.Decimal{}
	for _, d := range f.reserveCalls[0] {
		got[d.LimitID] = d.Amount
	}
	if !got[1].Equal(d("50000")) || !got[2].Equal(d("50000")) {
		t.Fatalf("scope debits: %+v", got)
	}
	if _, ok := got[3]; ok {
		t.Fatalf("unrelated USDJPY limit must not be debited: %+v", got)
	}

	// Limits exist but none apply to the pair → admit without a debit.
	f2 := &pbStoreFake{limits: []PBCreditLimit{pbLimits()[2]}} // USDJPY only
	svc = NewPBCreditService(f2, nil, nil)
	if err := svc.ReserveHeadroom(context.Background(), 100, 7,
		"EURUSD", "USD", d("1")); err != nil {
		t.Fatal(err)
	}
	if len(f2.reserveCalls) != 0 {
		t.Fatal("no applicable scope must not debit")
	}
}

func TestPBReserveHeadroomBreachCodes(t *testing.T) {
	for bound, want := range map[string]string{
		"NOP": "PB_NOP_LIMIT_EXCEEDED",
		"DSL": "PB_DSL_LIMIT_EXCEEDED",
	} {
		f := &pbStoreFake{
			limits: pbLimits(),
			reserveErr: &PBBreachError{Bound: bound, Scope: "GLOBAL", LimitID: 1,
				Limit: d("100"), Current: d("90"), Requested: d("20")},
		}
		svc := NewPBCreditService(f, nil, nil)
		err := svc.ReserveHeadroom(context.Background(), 100, 7,
			"EURUSD", "USD", d("20"))
		if err == nil {
			t.Fatalf("%s breach must reject", bound)
		}
		requireCode(t, err, want)
	}
	// Generic reserve failure → internal coded error.
	f := &pbStoreFake{limits: pbLimits(), reserveErr: fmt.Errorf("tx abort")}
	svc := NewPBCreditService(f, nil, nil)
	requireCode(t, svc.ReserveHeadroom(context.Background(), 100, 7,
		"EURUSD", "USD", d("1")), CodeRiskLimitsInternal)
}

// ---------------------------------------------------------------------------
// Release / consume / adjust lifecycle
// ---------------------------------------------------------------------------

func TestPBReleaseAndConsume(t *testing.T) {
	ctx := context.Background()
	f := &pbStoreFake{}
	svc := NewPBCreditService(f, nil, nil)

	if err := svc.ReleaseHeadroom(ctx, 555); err != nil {
		t.Fatal(err)
	}
	if len(f.releaseCalls) != 1 || f.releaseCalls[0] != 555 {
		t.Fatalf("release calls: %v", f.releaseCalls)
	}
	if err := svc.OnFill(ctx, 555); err != nil {
		t.Fatal(err)
	}
	if len(f.consumeCalls) != 1 || f.consumeCalls[0] != 555 {
		t.Fatalf("consume calls: %v", f.consumeCalls)
	}

	f.releaseErr = fmt.Errorf("pg down")
	requireCode(t, svc.ReleaseHeadroom(ctx, 555), CodeRiskLimitsInternal)
	f.releaseErr = nil
	f.consumeErr = fmt.Errorf("pg down")
	requireCode(t, svc.OnFill(ctx, 555), CodeRiskLimitsInternal)
	f.consumeErr = nil

	bare := NewPBCreditService(nil, nil, nil)
	requireCode(t, bare.ReleaseHeadroom(ctx, 1), CodeRiskLimitsInternal)
	requireCode(t, bare.OnFill(ctx, 1), CodeRiskLimitsInternal)
	requireCode(t, bare.AdjustHeadroom(ctx, 1, "EURUSD", "USD", d("1")), CodeRiskLimitsInternal)
	if _, err := bare.ResetDailySettled(ctx); err == nil {
		t.Fatal("nil store reset must reject")
	}
	if _, err := bare.SweepOrphans(ctx); err == nil {
		t.Fatal("nil store sweep must reject")
	}
	if _, err := bare.Utilization(ctx, 7); err == nil {
		t.Fatal("nil store utilization must reject")
	}
	if err := bare.SyncNetOpenPosition(ctx, 7); err == nil {
		t.Fatal("nil store sync must reject")
	}
}

func TestPBAdjustHeadroom(t *testing.T) {
	ctx := context.Background()
	// No ACTIVE reservation → amend is a no-op (AdjustTx never fires).
	f := &pbStoreFake{reservations: []PBReservation{
		{ID: 1, OrderID: 9, Status: "RELEASED"},
		{ID: 2, OrderID: 9, Status: "CONSUMED"},
	}}
	svc := NewPBCreditService(f, nil, nil)
	if err := svc.AdjustHeadroom(ctx, 9, "EURUSD", "USD", d("1000")); err != nil {
		t.Fatal(err)
	}
	if len(f.adjustCalls) != 0 {
		t.Fatalf("no ACTIVE row must skip AdjustTx: %v", f.adjustCalls)
	}

	// ACTIVE reservation + positive delta → re-checks limits via AdjustTx.
	f = &pbStoreFake{reservations: []PBReservation{{ID: 1, OrderID: 9, Status: "ACTIVE"}}}
	svc = NewPBCreditService(f, nil, nil)
	if err := svc.AdjustHeadroom(ctx, 9, "EURUSD", "USD", d("2500")); err != nil {
		t.Fatal(err)
	}
	if len(f.adjustCalls) != 1 || !f.adjustCalls[0].Equal(d("2500")) {
		t.Fatalf("adjust calls: %v", f.adjustCalls)
	}
	// Negative delta credits back.
	if err := svc.AdjustHeadroom(ctx, 9, "EURUSD", "USD", d("-800")); err != nil {
		t.Fatal(err)
	}
	if !f.adjustCalls[1].Equal(d("-800")) {
		t.Fatalf("negative adjust: %v", f.adjustCalls[1])
	}
	// Breach surfaces the PB codes.
	f.adjustErr = &PBBreachError{Bound: "NOP", Scope: "GLOBAL", LimitID: 1,
		Limit: d("1"), Current: d("1"), Requested: d("1")}
	requireCode(t, svc.AdjustHeadroom(ctx, 9, "EURUSD", "USD", d("1")),
		"PB_NOP_LIMIT_EXCEEDED")
}

// ---------------------------------------------------------------------------
// SyncNetOpenPosition / resets / sweep / utilization
// ---------------------------------------------------------------------------

func TestPBSyncNetOpenPosition(t *testing.T) {
	ctx := context.Background()
	f := &pbStoreFake{exposure: []PBInstrumentExposure{
		{InstrumentID: 1, CurrencyPair: "EURUSD", QuoteCurrency: "USD",
			SignedNotional: d("120000")},
		{InstrumentID: 2, CurrencyPair: "USDJPY", QuoteCurrency: "JPY",
			SignedNotional: d("-100000")},
	}}
	conv := position.NewConverter(pbRateFake{"JPY/USD": d("0.007")}, "")
	svc := NewPBCreditService(f, conv, nil)
	if err := svc.SyncNetOpenPosition(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if len(f.syncCalls) != 1 {
		t.Fatalf("sync calls: %+v", f.syncCalls)
	}
	c := f.syncCalls[0]
	// global = 120000 + (-100000 JPY × 0.007) = 120000 - 700 = 119300.
	if !c.globalUSD.Equal(d("119300")) {
		t.Fatalf("global USD=%s, want 119300", c.globalUSD)
	}
	if !c.pairUSD["EURUSD"].Equal(d("120000")) {
		t.Fatalf("pair EURUSD=%s", c.pairUSD["EURUSD"])
	}
	if !c.pairUSD["USDJPY"].Equal(d("-700")) {
		t.Fatalf("pair USDJPY=%s, want -700", c.pairUSD["USDJPY"])
	}
	// Missing conversion rate fails closed.
	f.exposure = []PBInstrumentExposure{
		{CurrencyPair: "EURGBP", QuoteCurrency: "GBP", SignedNotional: d("1")},
	}
	if err := svc.SyncNetOpenPosition(ctx, 7); err == nil {
		t.Fatal("missing FX rate must fail closed")
	}
}

func TestPBDailyResetAndSweep(t *testing.T) {
	ctx := context.Background()
	f := &pbStoreFake{resetN: 4, terminalN: 3}
	svc := NewPBCreditService(f, nil, nil)
	if n, err := svc.ResetDailySettled(ctx); err != nil || n != 4 {
		t.Fatalf("reset: %d %v", n, err)
	}
	if n, err := svc.SweepOrphans(ctx); err != nil || n != 3 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	f.resetErr = fmt.Errorf("pg down")
	if _, err := svc.ResetDailySettled(ctx); err == nil {
		t.Fatal("reset failure must surface")
	}
	f.terminalErr = fmt.Errorf("pg down")
	if _, err := svc.SweepOrphans(ctx); err == nil {
		t.Fatal("sweep failure must surface")
	}
}

func TestPBUtilizationAlertBand(t *testing.T) {
	nopLim, dslLim := d("10000000"), d("5000000")
	f := &pbStoreFake{limits: []PBCreditLimit{
		{ID: 1, NOPLimit: &nopLim, CurrentNOP: d("9000000"),
			DSLLimit: &dslLim, CurrentDSL: d("1000000")}, // NOP 90% — at the band
		{ID: 2, NOPLimit: &nopLim, CurrentNOP: d("1000000"),
			DSLLimit: &dslLim, CurrentDSL: d("4400000")}, // DSL 88% — under
		{ID: 3}, // uncapped — both pcts zero
	}}
	svc := NewPBCreditService(f, nil, nil)
	us, err := svc.Utilization(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(us) != 3 {
		t.Fatalf("rows: %+v", us)
	}
	if !us[0].NOPPct.Equal(d("90")) || !us[0].Alerted {
		t.Fatalf("row1: %+v — 90%% must alert (>= threshold)", us[0])
	}
	if us[1].Alerted || !us[1].DSLPct.Equal(d("88")) {
		t.Fatalf("row2: %+v — 88%% must not alert", us[1])
	}
	if us[2].NOPPct.IsPositive() || us[2].DSLPct.IsPositive() || us[2].Alerted {
		t.Fatalf("uncapped row: %+v", us[2])
	}
}

// ---------------------------------------------------------------------------
// toUSD — identity + conversion fail-closed
// ---------------------------------------------------------------------------

func TestPBToUSD(t *testing.T) {
	svc := NewPBCreditService(&pbStoreFake{}, nil, nil)
	got, err := svc.toUSD(context.Background(), d("-123.45"), "usd")
	if err != nil || !got.Equal(d("123.45")) {
		t.Fatalf("USD identity: %s %v", got, err)
	}
	conv := position.NewConverter(pbRateFake{"EUR/USD": d("1.20")}, "")
	svc = NewPBCreditService(&pbStoreFake{}, conv, nil)
	got, err = svc.toUSD(context.Background(), d("1000"), "EUR")
	if err != nil || !got.Equal(d("1200")) {
		t.Fatalf("EUR→USD: %s %v", got, err)
	}
	if _, err := svc.toUSD(context.Background(), d("100"), "ZZZ"); err == nil {
		t.Fatal("missing rate must fail closed")
	} else {
		requireCode(t, err, CodeRiskLimitsInternal)
	}
}

// ---------------------------------------------------------------------------
// Gated Redis — utilization mirror + 90% alert dedupe
// ---------------------------------------------------------------------------

func TestRedisPBCreditMirrorAndAlertDedup(t *testing.T) {
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("EXC_REDIS_TEST not set")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	rdb := excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 14)
	ctx := context.Background()
	if err := rdb.Ping(ctx); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}
	defer func() { _ = rdb.Close() }()

	nopLim := d("1000000")
	f := &pbStoreFake{limits: []PBCreditLimit{
		{ID: 1, PrimeBrokerID: 10, ClientAccountID: 77,
			NOPLimit: &nopLim, CurrentNOP: d("950000")}, // 95% — over the band
	}}
	svc := NewPBCreditService(f, nil, rdb)
	var fired []PBAlert
	svc.OnAlert = func(_ context.Context, a PBAlert) { fired = append(fired, a) }
	defer func() {
		_, _ = rdb.Del(ctx, "pb_credit_alert:1:NOP",
			fmt.Sprintf("pb_credit:%d:%d", 10, 77)).Result()
	}()

	// ReserveHeadroom commits (fake) then mirrors fresh utilization and
	// fires the ≥90% alert once (SetNX dedupe).
	if err := svc.ReserveHeadroom(ctx, 500, 77, "EURUSD", "USD", d("50000")); err != nil {
		t.Fatal(err)
	}
	if len(fired) != 1 || fired[0].Bound != "NOP" {
		t.Fatalf("alert fired=%v, want exactly one NOP alert", fired)
	}
	// Second reserve → dedupe suppresses the repeat page.
	if err := svc.ReserveHeadroom(ctx, 501, 77, "EURUSD", "USD", d("50000")); err != nil {
		t.Fatal(err)
	}
	if len(fired) != 1 {
		t.Fatalf("dedupe failed, fired=%d", len(fired))
	}
	// The utilization mirror hash carries the scope fields.
	vals, err := rdb.HGetAll(ctx, "pb_credit:10:77").Result()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vals["global"]; !ok {
		t.Fatalf("mirror missing global scope: %v", vals)
	}
	if !strings.Contains(vals["global"], `"nop_current":"950000"`) {
		t.Fatalf("mirror payload: %v", vals["global"])
	}
}

// ---------------------------------------------------------------------------
// Gated PostgreSQL — PgPBCreditStore over the real schema
// ---------------------------------------------------------------------------

func pbSeed(t *testing.T, pool *pgxpool.Pool) (pbID, acct int64) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `
		INSERT INTO prime_brokers (pb_name, bic_code, fix_comp_id)
		VALUES ($1, $2, $3) RETURNING id`,
		"IT PB", "ITPBUS33", fmt.Sprintf("PB%d", time.Now().UnixNano())).Scan(&pbID); err != nil {
		t.Fatalf("pb seed: %v", err)
	}
	acct = liqSeedAccount(t, pool, "PROFESSIONAL")
	return pbID, acct
}

func pbSeedLimit(t *testing.T, pool *pgxpool.Pool, pbID, acct int64,
	pair *string, nop, dsl string) PBCreditLimit {
	t.Helper()
	st := NewPgPBCreditStore(pool)
	var nopD, dslD *decimal.Decimal
	if nop != "" {
		v := d(nop)
		nopD = &v
	}
	if dsl != "" {
		v := d(dsl)
		dslD = &v
	}
	l, err := st.UpsertLimit(context.Background(), PBCreditLimit{
		PrimeBrokerID: pbID, ClientAccountID: acct,
		CurrencyPair: pair, NOPLimit: nopD, DSLLimit: dslD,
	})
	if err != nil {
		t.Fatalf("upsert limit: %v", err)
	}
	return l
}

func TestPgPBCreditStoreReserveRelease(t *testing.T) {
	pool := pgRisk19Fixture(t)
	ctx := context.Background()
	st := NewPgPBCreditStore(pool)
	pbID, acct := pbSeed(t, pool)
	global := pbSeedLimit(t, pool, pbID, acct, nil, "1000000", "2000000")
	pair := pbSeedLimit(t, pool, pbID, acct, strp("EURUSD"), "500000", "")

	limits, err := st.LimitsFor(ctx, acct)
	if err != nil || len(limits) != 2 {
		t.Fatalf("limits: %v %d", err, len(limits))
	}

	// Atomic debit: both counters + one reservation row each.
	debits := []PBDebit{
		{LimitID: global.ID, Amount: d("100000")},
		{LimitID: pair.ID, Amount: d("100000")},
	}
	if err := st.ReserveTx(ctx, 9001, acct, debits); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	limits, _ = st.LimitsFor(ctx, acct)
	for _, l := range limits {
		if !l.CurrentNOP.Equal(d("100000")) || !l.CurrentDSL.Equal(d("100000")) {
			t.Fatalf("counters after reserve: %+v", l)
		}
	}
	res, err := st.ReservationsFor(ctx, 9001)
	if err != nil || len(res) != 2 {
		t.Fatalf("reservations: %v %d", err, len(res))
	}
	for _, r := range res {
		if r.Status != "ACTIVE" {
			t.Fatalf("reservation status: %+v", r)
		}
	}

	// Release credits both counters back; second release is a no-op.
	rels, err := st.ReleaseTx(ctx, 9001)
	if err != nil || len(rels) != 2 {
		t.Fatalf("release: %v %d", err, len(rels))
	}
	limits, _ = st.LimitsFor(ctx, acct)
	for _, l := range limits {
		if !l.CurrentNOP.IsZero() || !l.CurrentDSL.IsZero() {
			t.Fatalf("counters after release: %+v", l)
		}
	}
	rels, err = st.ReleaseTx(ctx, 9001)
	if err != nil || len(rels) != 0 {
		t.Fatalf("release must be idempotent: %v %d", err, len(rels))
	}
}

func TestPgPBCreditStoreBreachAtomic(t *testing.T) {
	pool := pgRisk19Fixture(t)
	ctx := context.Background()
	st := NewPgPBCreditStore(pool)
	pbID, acct := pbSeed(t, pool)
	global := pbSeedLimit(t, pool, pbID, acct, nil, "1000000", "2000000")
	pair := pbSeedLimit(t, pool, pbID, acct, strp("EURUSD"), "500000", "1500000")

	// NOP breach on the pair row → PBBreachError, NOTHING debited.
	err := st.ReserveTx(ctx, 9002, acct, []PBDebit{
		{LimitID: global.ID, Amount: d("100000")},
		{LimitID: pair.ID, Amount: d("600000")},
	})
	var be *PBBreachError
	if !errors.As(err, &be) {
		t.Fatalf("want PBBreachError, got %v", err)
	}
	if be.Bound != "NOP" || be.Scope != "EURUSD" {
		t.Fatalf("breach: %+v", be)
	}
	limits, _ := st.LimitsFor(ctx, acct)
	for _, l := range limits {
		if !l.CurrentNOP.IsZero() || !l.CurrentDSL.IsZero() {
			t.Fatalf("breach must leave counters untouched: %+v", l)
		}
	}
	res, _ := st.ReservationsFor(ctx, 9002)
	if len(res) != 0 {
		t.Fatalf("breach must not leave reservations: %+v", res)
	}

	// DSL breach: NOP has headroom but the request crosses the settled
	// bound (NOP 2M / DSL 1M scope → 1.5M trips DSL, not NOP).
	jpy := pbSeedLimit(t, pool, pbID, acct, strp("USDJPY"), "2000000", "1000000")
	err = st.ReserveTx(ctx, 9003, acct, []PBDebit{
		{LimitID: jpy.ID, Amount: d("1500000")},
	})
	if !errors.As(err, &be) || be.Bound != "DSL" {
		t.Fatalf("want DSL breach, got %v", err)
	}
	if be.Scope != "USDJPY" {
		t.Fatalf("dsl breach scope: %+v", be)
	}
}

func TestPgPBCreditStoreConsumeAdjustSweep(t *testing.T) {
	pool := pgRisk19Fixture(t)
	ctx := context.Background()
	st := NewPgPBCreditStore(pool)
	pbID, acct := pbSeed(t, pool)
	global := pbSeedLimit(t, pool, pbID, acct, nil, "1000000", "2000000")

	if err := st.ReserveTx(ctx, 9004, acct, []PBDebit{
		{LimitID: global.ID, Amount: d("200000")},
	}); err != nil {
		t.Fatal(err)
	}
	// Consume → status flips, counters KEEP the debit (position carries it).
	if err := st.ConsumeTx(ctx, 9004); err != nil {
		t.Fatal(err)
	}
	res, _ := st.ReservationsFor(ctx, 9004)
	if len(res) != 1 || res[0].Status != "CONSUMED" {
		t.Fatalf("consume: %+v", res)
	}
	limits, _ := st.LimitsFor(ctx, acct)
	if !limits[0].CurrentNOP.Equal(d("200000")) {
		t.Fatalf("consumed counters must stay debited: %+v", limits[0])
	}
	// Release on a consumed order does not credit back.
	rels, _ := st.ReleaseTx(ctx, 9004)
	if len(rels) != 0 {
		t.Fatalf("consumed reservation must not release: %+v", rels)
	}
	limits, _ = st.LimitsFor(ctx, acct)
	if !limits[0].CurrentNOP.Equal(d("200000")) {
		t.Fatalf("consumed release must not credit: %+v", limits[0])
	}

	// Adjust — re-reserve then size up/down.
	if err := st.ReserveTx(ctx, 9005, acct, []PBDebit{
		{LimitID: global.ID, Amount: d("100000")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AdjustTx(ctx, 9005, d("50000")); err != nil {
		t.Fatal(err)
	}
	limits, _ = st.LimitsFor(ctx, acct)
	if !limits[0].CurrentNOP.Equal(d("350000")) {
		t.Fatalf("adjust up: %+v", limits[0])
	}
	if err := st.AdjustTx(ctx, 9005, d("-70000")); err != nil {
		t.Fatal(err)
	}
	limits, _ = st.LimitsFor(ctx, acct)
	if !limits[0].CurrentNOP.Equal(d("280000")) {
		t.Fatalf("adjust down: %+v", limits[0])
	}
	// Adjust beyond the limit breaches; nothing applies.
	var be *PBBreachError
	if err := st.AdjustTx(ctx, 9005, d("900000")); !errors.As(err, &be) {
		t.Fatalf("adjust breach: %v", err)
	}
	limits, _ = st.LimitsFor(ctx, acct)
	if !limits[0].CurrentNOP.Equal(d("280000")) {
		t.Fatalf("breached adjust must not apply: %+v", limits[0])
	}

	// Terminal sweep: a reservation on a CANCELLED order releases.
	var orderID int64
	inst := liqSeedInstrument(t, pool)
	if err := pool.QueryRow(ctx, `
		INSERT INTO orders (account_id, instrument_id, side, order_type,
		                    quantity, time_in_force, status)
		VALUES ($1,$2,'BUY','LIMIT',100,'GTC','CANCELLED') RETURNING id`,
		acct, inst).Scan(&orderID); err != nil {
		t.Fatalf("order seed: %v", err)
	}
	if err := st.ReserveTx(ctx, orderID, acct, []PBDebit{
		{LimitID: global.ID, Amount: d("50000")},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := st.ReleaseTerminalTx(ctx)
	if err != nil || n != 1 {
		t.Fatalf("terminal sweep: %d %v", n, err)
	}
	limits, _ = st.LimitsFor(ctx, acct)
	if !limits[0].CurrentNOP.Equal(d("280000")) {
		t.Fatalf("sweep must credit the orphan back: %+v", limits[0])
	}

	// Daily reset zeroes the DSL counters only.
	m, err := st.ResetDailySettled(ctx)
	if err != nil || m == 0 {
		t.Fatalf("reset: %d %v", m, err)
	}
	limits, _ = st.LimitsFor(ctx, acct)
	for _, l := range limits {
		if !l.CurrentDSL.IsZero() {
			t.Fatalf("dsl not reset: %+v", l)
		}
	}
	if !limits[0].CurrentNOP.Equal(d("280000")) {
		t.Fatalf("nop must survive reset: %+v", limits[0])
	}
}

func TestPgPBCreditStoreSyncNOP(t *testing.T) {
	pool := pgRisk19Fixture(t)
	ctx := context.Background()
	st := NewPgPBCreditStore(pool)
	svc := NewPBCreditService(st, nil, nil)
	pbID, acct := pbSeed(t, pool)
	global := pbSeedLimit(t, pool, pbID, acct, nil, "10000000", "")
	pair := pbSeedLimit(t, pool, pbID, acct, strp("EURUSD"), "5000000", "")

	// Booked exposure: +120K EURUSD long, -50K EURUSD short → net 70K.
	// quantity is unsigned; side carries the direction (PositionExposure
	// signs via CASE side → -quantity for shorts).
	inst := liqSeedInstrument(t, pool)
	liqSeedPosition(t, pool, acct, inst, "LONG", "100000", "1.20",
		liqStr("1.20"), nil, "0", "0")
	liqSeedPosition(t, pool, acct, inst, "SHORT", "50000", "1.20",
		liqStr("1.00"), nil, "0", "0")
	// One active reservation still counts toward utilization.
	if err := st.ReserveTx(ctx, 9010, acct, []PBDebit{
		{LimitID: global.ID, Amount: d("30000")},
		{LimitID: pair.ID, Amount: d("30000")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SyncNetOpenPosition(ctx, acct); err != nil {
		t.Fatal(err)
	}
	limits, err := st.LimitsFor(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	// PositionExposure sums signed qty×mark: +100000×1.20 - 50000×1.00
	// = 120000 - 50000 = 70000 booked net. Plus 30K active reservation
	// per scope → |70000| + 30000 = 100000 on both rows.
	var g, p PBCreditLimit
	for _, l := range limits {
		if l.ID == global.ID {
			g = l
		}
		if l.ID == pair.ID {
			p = l
		}
	}
	if !g.CurrentNOP.Equal(d("100000")) {
		t.Fatalf("global nop=%s, want 100000 (70K booked + 30K res)", g.CurrentNOP)
	}
	if !p.CurrentNOP.Equal(d("100000")) {
		t.Fatalf("pair nop=%s, want 100000", p.CurrentNOP)
	}
}
