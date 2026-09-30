// Unit tests for Task 3.3.5 risk limits enforcement. All persistence is
// faked — no DB/Redis needed; pgx/Redis paths are exercised by the gated
// integration suites (EXC_REDIS_TEST=1 / dev compose).
package risk

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"testing"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeStore struct {
	rows      []Row
	usage     map[string]Usage // "acct|YYYYMMDD"
	open      map[int64]int64
	exposures map[int64][]SymbolExposure
	// withdrawn maps accountID → rolling-window withdrawal sum served
	// to WithdrawnSince; venueWithdrawn serves VenueWithdrawnSince.
	// Tests set them directly — the fake does not model windows.
	withdrawn      map[int64]decimal.Decimal
	venueWithdrawn decimal.Decimal
	err            error // injected store failure
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		usage:     map[string]Usage{},
		open:      map[int64]int64{},
		exposures: map[int64][]SymbolExposure{},
		withdrawn: map[int64]decimal.Decimal{},
	}
}

func usageKey(accountID int64, day time.Time) string {
	return fmt.Sprintf("%d|%s", accountID, day.Format("20060102"))
}

func (f *fakeStore) LoadLimits(context.Context) ([]Row, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func (f *fakeStore) DailyUsage(_ context.Context, accountID int64, day time.Time) (Usage, error) {
	if f.err != nil {
		return Usage{}, f.err
	}
	return f.usage[usageKey(accountID, day)], nil
}

func (f *fakeStore) AddDailyVolume(_ context.Context, accountID int64, day time.Time, delta decimal.Decimal) (decimal.Decimal, error) {
	if f.err != nil {
		return decimal.Zero, f.err
	}
	k := usageKey(accountID, day)
	u := f.usage[k]
	u.Volume = u.Volume.Add(delta)
	f.usage[k] = u
	return u.Volume, nil
}

func (f *fakeStore) AddDailyWithdrawn(_ context.Context, accountID int64, day time.Time, delta decimal.Decimal) (decimal.Decimal, error) {
	if f.err != nil {
		return decimal.Zero, f.err
	}
	k := usageKey(accountID, day)
	u := f.usage[k]
	u.Withdrawn = u.Withdrawn.Add(delta)
	f.usage[k] = u
	return u.Withdrawn, nil
}

func (f *fakeStore) OpenOrderCount(_ context.Context, accountID int64) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.open[accountID], nil
}

func (f *fakeStore) SymbolExposures(_ context.Context, accountID int64) ([]SymbolExposure, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.exposures[accountID], nil
}

func (f *fakeStore) WithdrawnSince(_ context.Context, accountID int64, _ time.Time) (decimal.Decimal, error) {
	if f.err != nil {
		return decimal.Zero, f.err
	}
	if v, ok := f.withdrawn[accountID]; ok {
		return v, nil
	}
	return decimal.Zero, nil
}

func (f *fakeStore) VenueWithdrawnSince(_ context.Context, _ time.Time) (decimal.Decimal, error) {
	if f.err != nil {
		return decimal.Zero, f.err
	}
	return f.venueWithdrawn, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

var testDay = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

func svcWith(t *testing.T, fs *fakeStore, clock func() time.Time) *LimitsService {
	t.Helper()
	s := NewLimitsService(fs, nil, clock)
	if err := s.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s
}

func dec(v string) *decimal.Decimal {
	d := decimal.MustFromString(v)
	return &d
}

func int64p(v int64) *int64 { return &v }
func int32p(v int32) *int32 { return &v }
func strp(v string) *string { return &v }

func requireCode(t *testing.T, err error, want string) {
	t.Helper()
	var e *excerrors.Error
	if !stderrors.As(err, &e) {
		t.Fatalf("err %v is not a coded *errors.Error", err)
	}
	if e.Code != want {
		t.Fatalf("code = %q, want %q", e.Code, want)
	}
}

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

func TestResolvePrecedence(t *testing.T) {
	// Global < tier < account < account+symbol, per column.
	rows := []Row{
		{ID: 1, MaxOrderQty: dec("1000"), MaxDailyVolume: dec("100000")},                   // global
		{ID: 2, Tier: strp("T1"), MaxOrderQty: dec("2000")},                                // tier T1
		{ID: 3, AccountID: int64p(7), MaxOrderQty: dec("3000"), MaxOpenOrders: int32p(50)}, // account
		{ID: 4, AccountID: int64p(7), Symbol: strp("EURUSD"), MaxOrderQty: dec("4000")},    // account+symbol
	}
	lim := Resolve(rows, 7, "T1", "EURUSD")
	if got := lim.MaxOrderQty.String(); got != "4000" {
		t.Fatalf("MaxOrderQty = %s, want 4000 (account+symbol row wins)", got)
	}
	if got := lim.MaxDailyVolume.String(); got != "100000" {
		t.Fatalf("MaxDailyVolume = %s, want 100000 (global fallback)", got)
	}
	if *lim.MaxOpenOrders != 50 {
		t.Fatalf("MaxOpenOrders = %d, want 50", *lim.MaxOpenOrders)
	}

	// Other symbol: account+symbol row does not apply; account row wins qty.
	lim = Resolve(rows, 7, "T1", "GBPUSD")
	if got := lim.MaxOrderQty.String(); got != "3000" {
		t.Fatalf("GBPUSD MaxOrderQty = %s, want 3000", got)
	}
	// Other account, tier T1: tier row wins over global for qty.
	lim = Resolve(rows, 9, "T1", "EURUSD")
	if got := lim.MaxOrderQty.String(); got != "2000" {
		t.Fatalf("T1 MaxOrderQty = %s, want 2000", got)
	}
	// Tier mismatch: global applies.
	lim = Resolve(rows, 9, "T2", "EURUSD")
	if got := lim.MaxOrderQty.String(); got != "1000" {
		t.Fatalf("T2 MaxOrderQty = %s, want 1000", got)
	}
	// Symbol '*' row matches any symbol.
	rows = append(rows, Row{ID: 5, AccountID: int64p(9), Symbol: strp("*"), MaxOrderQty: dec("5000")})
	lim = Resolve(rows, 9, "T2", "USDJPY")
	if got := lim.MaxOrderQty.String(); got != "5000" {
		t.Fatalf("wildcard MaxOrderQty = %s, want 5000", got)
	}
}

func TestResolveExposureDefaults(t *testing.T) {
	// No rows → spec §13.6 canonical defaults, other fields unlimited.
	lim := Resolve(nil, 1, "T0", "EURUSD")
	if lim.MaxOrderQty != nil || lim.MaxDailyVolume != nil || lim.MaxOpenOrders != nil {
		t.Fatal("unset columns must be nil (unlimited)")
	}
	if !lim.MaxNotionalExposure.Equal(DefaultSymbolNotionalCap) {
		t.Fatalf("MaxNotionalExposure = %s, want %s", lim.MaxNotionalExposure, DefaultSymbolNotionalCap)
	}
	if !lim.MaxShortExposure.Equal(DefaultShortNotionalCap) {
		t.Fatalf("MaxShortExposure = %s, want %s", lim.MaxShortExposure, DefaultShortNotionalCap)
	}
	if !lim.MaxAccountNotional.Equal(DefaultAccountNotionalCap) {
		t.Fatalf("MaxAccountNotional = %s, want %s", lim.MaxAccountNotional, DefaultAccountNotionalCap)
	}
}

// ---------------------------------------------------------------------------
// CheckOrder enforcement
// ---------------------------------------------------------------------------

func TestCheckOrderQtyBoundary(t *testing.T) {
	fs := newFakeStore()
	fs.rows = []Row{{ID: 1, AccountID: int64p(1), MaxOrderQty: dec("100"),
		MaxNotionalExposure: dec("999999999"), MaxShortExposure: dec("999999999"),
		MaxAccountNotional: dec("999999999")}}
	s := svcWith(t, fs, func() time.Time { return testDay })
	err := s.CheckOrder(context.Background(), OrderRequest{
		AccountID: 1, KycTier: "T1", Symbol: "EURUSD", Side: "BUY",
		Quantity: decimal.NewFromInt(100), Price: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("qty == max must pass: %v", err)
	}
	err = s.CheckOrder(context.Background(), OrderRequest{
		AccountID: 1, KycTier: "T1", Symbol: "EURUSD", Side: "BUY",
		Quantity: decimal.NewFromInt(101), Price: decimal.NewFromInt(1),
	})
	requireCode(t, err, CodeOrderRejected)
}

func TestCheckOrderDailyVolume(t *testing.T) {
	fs := newFakeStore()
	fs.rows = []Row{{ID: 1, AccountID: int64p(1), MaxDailyVolume: dec("1000"),
		MaxNotionalExposure: dec("999999999"), MaxAccountNotional: dec("999999999")}}
	s := svcWith(t, fs, func() time.Time { return testDay })
	fs.usage[usageKey(1, testDay)] = Usage{Volume: decimal.NewFromInt(900)}

	// 900 + 100 = 1000 == cap → allowed (limit is exclusive breach).
	err := s.CheckOrder(context.Background(), OrderRequest{
		AccountID: 1, KycTier: "T1", Symbol: "EURUSD", Side: "BUY",
		Quantity: decimal.NewFromInt(50), Price: decimal.NewFromInt(2), // notional 100
	})
	if err != nil {
		t.Fatalf("volume exactly at cap must pass: %v", err)
	}
	// 900 + 200 > 1000 → reject.
	err = s.CheckOrder(context.Background(), OrderRequest{
		AccountID: 1, KycTier: "T1", Symbol: "EURUSD", Side: "BUY",
		Quantity: decimal.NewFromInt(100), Price: decimal.NewFromInt(2),
	})
	requireCode(t, err, CodeOrderRejected)
}

func TestDailyResetBoundary(t *testing.T) {
	fs := newFakeStore()
	fs.rows = []Row{{ID: 1, AccountID: int64p(1), MaxDailyVolume: dec("1000"),
		MaxNotionalExposure: dec("999999999"), MaxAccountNotional: dec("999999999")}}
	now := testDay.Add(20 * time.Hour) // 2026-09-30 20:00 UTC
	s := svcWith(t, fs, func() time.Time { return now })

	if _, err := s.RecordFill(context.Background(), 1, decimal.NewFromInt(900)); err != nil {
		t.Fatal(err)
	}
	err := s.CheckOrder(context.Background(), OrderRequest{
		AccountID: 1, KycTier: "T1", Symbol: "EURUSD", Side: "BUY",
		Quantity: decimal.NewFromInt(100), Price: decimal.NewFromInt(2),
	})
	requireCode(t, err, CodeOrderRejected) // 900 + 200 > 1000

	// Clock crosses 00:00 UTC → the (account, day) key rolls to a fresh
	// row; the counter is structurally reset.
	now = testDay.AddDate(0, 0, 1) // 2026-10-01 00:00 UTC
	if err := s.CheckOrder(context.Background(), OrderRequest{
		AccountID: 1, KycTier: "T1", Symbol: "EURUSD", Side: "BUY",
		Quantity: decimal.NewFromInt(100), Price: decimal.NewFromInt(2),
	}); err != nil {
		t.Fatalf("post-reset order must pass: %v", err)
	}
	// Prior-day total is still on record (audit/history intact).
	if got := fs.usage[usageKey(1, testDay)].Volume; got.String() != "900" {
		t.Fatalf("prior-day volume = %s, want 900", got)
	}
}

func TestCheckOrderOpenOrders(t *testing.T) {
	fs := newFakeStore()
	fs.rows = []Row{{ID: 1, AccountID: int64p(1), MaxOpenOrders: int32p(5),
		MaxNotionalExposure: dec("999999999"), MaxAccountNotional: dec("999999999")}}
	s := svcWith(t, fs, func() time.Time { return testDay })
	fs.open[1] = 5

	err := s.CheckOrder(context.Background(), OrderRequest{
		AccountID: 1, KycTier: "T1", Symbol: "EURUSD", Side: "BUY",
		Quantity: decimal.NewFromInt(1), Price: decimal.NewFromInt(1),
	})
	requireCode(t, err, CodeOrderRejected) // 5 + 1 > 5

	fs.open[1] = 4
	if err := s.CheckOrder(context.Background(), OrderRequest{
		AccountID: 1, KycTier: "T1", Symbol: "EURUSD", Side: "BUY",
		Quantity: decimal.NewFromInt(1), Price: decimal.NewFromInt(1),
	}); err != nil {
		t.Fatalf("open 4 + 1 <= 5 must pass: %v", err)
	}
}

func TestCheckOrderSymbolExposure(t *testing.T) {
	fs := newFakeStore()
	fs.rows = []Row{
		{ID: 1, AccountID: int64p(1), MaxAccountNotional: dec("50000")}, // account-wide
		{ID: 2, AccountID: int64p(1), Symbol: strp("EURUSD"),
			MaxNotionalExposure: dec("10000"), MaxShortExposure: dec("4000")}, // per-symbol
	}
	s := svcWith(t, fs, func() time.Time { return testDay })
	fs.exposures[1] = []SymbolExposure{
		{Symbol: "EURUSD", GrossNotional: decimal.NewFromInt(9000),
			ShortNotional: decimal.NewFromInt(3000)},
		{Symbol: "GBPUSD", GrossNotional: decimal.NewFromInt(30000)},
	}

	// BUY: 9000 + 2000 > 10000 symbol gross cap → MAX_EXPOSURE_EXCEEDED.
	err := s.CheckOrder(context.Background(), OrderRequest{
		AccountID: 1, KycTier: "T1", Symbol: "EURUSD", Side: "BUY",
		Quantity: decimal.NewFromInt(1000), Price: decimal.NewFromInt(2),
	})
	requireCode(t, err, CodeMaxExposureExceeded)

	// SELL: short 3000 + 2000 > 4000 → MAX_EXPOSURE_EXCEEDED.
	err = s.CheckOrder(context.Background(), OrderRequest{
		AccountID: 1, KycTier: "T1", Symbol: "EURUSD", Side: "SELL",
		Quantity: decimal.NewFromInt(1000), Price: decimal.NewFromInt(2),
	})
	requireCode(t, err, CodeMaxExposureExceeded)

	// Account-wide: EURUSD small add but account gross 39000 + notional
	// > 50000 cap.
	err = s.CheckOrder(context.Background(), OrderRequest{
		AccountID: 1, KycTier: "T1", Symbol: "GBPUSD", Side: "BUY",
		Quantity: decimal.NewFromInt(10000), Price: decimal.NewFromInt(2), // +20000
	})
	requireCode(t, err, CodeMaxExposureExceeded)

	// Within all caps → pass.
	if err := s.CheckOrder(context.Background(), OrderRequest{
		AccountID: 1, KycTier: "T1", Symbol: "EURUSD", Side: "BUY",
		Quantity: decimal.NewFromInt(500), Price: decimal.NewFromInt(2), // +1000 → 10000 == cap
	}); err != nil {
		t.Fatalf("exposure at cap boundary must pass: %v", err)
	}
}

func TestCheckOrderReduceOnly(t *testing.T) {
	fs := newFakeStore()
	fs.rows = []Row{{ID: 1, AccountID: int64p(1), Symbol: strp("EURUSD"),
		MaxNotionalExposure: dec("100")}}
	s := svcWith(t, fs, func() time.Time { return testDay })
	fs.exposures[1] = []SymbolExposure{
		{Symbol: "EURUSD", GrossNotional: decimal.NewFromInt(200)}, // over cap already
	}
	// Reduce-only orders never add exposure — must pass even with no price.
	if err := s.CheckOrder(context.Background(), OrderRequest{
		AccountID: 1, KycTier: "T1", Symbol: "EURUSD", Side: "SELL",
		Quantity: decimal.NewFromInt(50), ReduceOnly: true,
	}); err != nil {
		t.Fatalf("reduce-only must bypass exposure checks: %v", err)
	}
}

func TestCheckOrderMissingPriceFailsClosed(t *testing.T) {
	fs := newFakeStore() // §13.6 default exposure caps apply — notional needed
	s := svcWith(t, fs, func() time.Time { return testDay })
	err := s.CheckOrder(context.Background(), OrderRequest{
		AccountID: 1, KycTier: "T1", Symbol: "EURUSD", Side: "BUY",
		Quantity: decimal.NewFromInt(1), // Price zero
	})
	requireCode(t, err, CodeOrderRejected)
}

func TestCheckOrderStoreErrorFailsClosed(t *testing.T) {
	fs := newFakeStore()
	fs.rows = []Row{{ID: 1, AccountID: int64p(1), MaxDailyVolume: dec("1000")}}
	s := svcWith(t, fs, func() time.Time { return testDay })
	fs.err = stderrors.New("pg down")
	err := s.CheckOrder(context.Background(), OrderRequest{
		AccountID: 1, KycTier: "T1", Symbol: "EURUSD", Side: "BUY",
		Quantity: decimal.NewFromInt(1), Price: decimal.NewFromInt(1),
	})
	if err == nil {
		t.Fatal("store error must reject (fail-closed)")
	}
	var e *excerrors.Error
	if !stderrors.As(err, &e) {
		t.Fatalf("want coded error, got %v", err)
	}
}

func TestCheckWithdrawal(t *testing.T) {
	fs := newFakeStore()
	fs.rows = []Row{{ID: 1, AccountID: int64p(1),
		MaxWithdrawAmount: dec("5000"), DailyWithdrawLimit: dec("20000")}}
	s := svcWith(t, fs, func() time.Time { return testDay })
	fs.usage[usageKey(1, testDay)] = Usage{Withdrawn: decimal.NewFromInt(15000)}

	// Per-transaction cap.
	err := s.CheckWithdrawal(context.Background(), 1, "T1", decimal.NewFromInt(5001))
	requireCode(t, err, CodeOrderRejected)
	// Daily cap: 15000 + 6000 > 20000.
	err = s.CheckWithdrawal(context.Background(), 1, "T1", decimal.NewFromInt(6000))
	requireCode(t, err, CodeOrderRejected)
	// Boundary: 15000 + 5000 == 20000 → allowed.
	if err := s.CheckWithdrawal(context.Background(), 1, "T1", decimal.NewFromInt(5000)); err != nil {
		t.Fatalf("withdrawal at cap boundary must pass: %v", err)
	}
	if _, err := s.RecordWithdrawal(context.Background(), 1, decimal.NewFromInt(5000)); err != nil {
		t.Fatal(err)
	}
	err = s.CheckWithdrawal(context.Background(), 1, "T1", decimal.NewFromInt(1))
	requireCode(t, err, CodeOrderRejected) // now at cap
}

func TestCheckWithdrawalHourlyRate(t *testing.T) {
	fs := newFakeStore()
	fs.rows = []Row{{ID: 1, AccountID: int64p(1),
		WithdrawRatePerHour: dec("10000")}}
	s := svcWith(t, fs, func() time.Time { return testDay.Add(12 * time.Hour) })
	fs.withdrawn[1] = decimal.NewFromInt(8000)

	// 8000 + 3000 > 10000 → ORDER_REJECTED.
	err := s.CheckWithdrawal(context.Background(), 1, "T1", decimal.NewFromInt(3000))
	requireCode(t, err, CodeOrderRejected)
	// Boundary: 8000 + 2000 == 10000 → allowed (exclusive breach).
	if err := s.CheckWithdrawal(context.Background(), 1, "T1",
		decimal.NewFromInt(2000)); err != nil {
		t.Fatalf("hourly sum exactly at cap must pass: %v", err)
	}
	// Unset cap → hourly usage never consulted... but the cap being nil
	// means the window sum is irrelevant; a pass proves no rejection.
	fs2 := newFakeStore()
	fs2.rows = []Row{{ID: 1, AccountID: int64p(1)}}
	fs2.withdrawn[1] = decimal.NewFromInt(999999999)
	s2 := svcWith(t, fs2, func() time.Time { return testDay })
	if err := s2.CheckWithdrawal(context.Background(), 1, "T1",
		decimal.NewFromInt(1)); err != nil {
		t.Fatalf("unset withdraw_rate_per_hour must not reject: %v", err)
	}
	// Fail closed: a window-read error rejects the withdrawal.
	fs3 := newFakeStore()
	fs3.rows = []Row{{ID: 1, AccountID: int64p(1),
		WithdrawRatePerHour: dec("10000")}}
	s3 := svcWith(t, fs3, func() time.Time { return testDay })
	fs3.err = stderrors.New("pg down") // Load succeeded; reads now fail
	requireCode(t, s3.CheckWithdrawal(context.Background(), 1, "T1",
		decimal.NewFromInt(1)), CodeRiskLimitsInternal)
}

func TestCheckWithdrawalVenueDailyCap(t *testing.T) {
	fs := newFakeStore()
	// Only the fully-global row carries the venue ceiling.
	fs.rows = []Row{
		{ID: 1, ExchangeDailyWithdrawLimit: dec("100000")},
		{ID: 2, AccountID: int64p(1)}, // scoped row cannot widen/narrow it
	}
	s := svcWith(t, fs, func() time.Time { return testDay.Add(12 * time.Hour) })
	fs.venueWithdrawn = decimal.NewFromInt(99000)

	// 99000 + 2000 > 100000 → ORDER_REJECTED.
	err := s.CheckWithdrawal(context.Background(), 1, "T1", decimal.NewFromInt(2000))
	requireCode(t, err, CodeOrderRejected)
	// Boundary: 99000 + 1000 == 100000 → allowed.
	if err := s.CheckWithdrawal(context.Background(), 1, "T1",
		decimal.NewFromInt(1000)); err != nil {
		t.Fatalf("venue sum exactly at cap must pass: %v", err)
	}
	// Other accounts hit the same venue ceiling.
	if err := s.CheckWithdrawal(context.Background(), 9, "T2",
		decimal.NewFromInt(2000)); err == nil {
		t.Fatal("venue cap must apply account-independently")
	}
	// Fail closed on the venue-sum read.
	fs.err = stderrors.New("pg down")
	requireCode(t, s.CheckWithdrawal(context.Background(), 1, "T1",
		decimal.NewFromInt(1)), CodeRiskLimitsInternal)
}

func TestResolveExchangeDailyCapGlobalOnly(t *testing.T) {
	rows := []Row{
		// Scoped rows setting the venue column are ignored.
		{ID: 1, AccountID: int64p(7), ExchangeDailyWithdrawLimit: dec("1")},
		{ID: 2, Tier: strp("T1"), ExchangeDailyWithdrawLimit: dec("2")},
		{ID: 3, Symbol: strp("EURUSD"), ExchangeDailyWithdrawLimit: dec("3")},
		// Fully-global rows — lowest id wins.
		{ID: 4, ExchangeDailyWithdrawLimit: dec("500000")},
		{ID: 5, ExchangeDailyWithdrawLimit: dec("999999")},
	}
	lim := Resolve(rows, 7, "T1", "EURUSD")
	if got := lim.ExchangeDailyWithdrawLimit.String(); got != "500000" {
		t.Fatalf("ExchangeDailyWithdrawLimit = %s, want 500000 (global row id 4)", got)
	}
	// No global row → unset (unlimited), even when scoped rows carry it.
	lim = Resolve(rows[:3], 7, "T1", "EURUSD")
	if lim.ExchangeDailyWithdrawLimit != nil {
		t.Fatal("scoped-row venue cap must not resolve")
	}
	// Wildcard-symbol global row counts as venue scope.
	rows2 := []Row{{ID: 6, Symbol: strp("*"),
		ExchangeDailyWithdrawLimit: dec("42000")}}
	lim = Resolve(rows2, 1, "T0", "GBPUSD")
	if got := lim.ExchangeDailyWithdrawLimit.String(); got != "42000" {
		t.Fatalf("wildcard global row venue cap = %s, want 42000", got)
	}
}

// ---------------------------------------------------------------------------
// LimitsView (API surface)
// ---------------------------------------------------------------------------

func TestLimitsViewUtilization(t *testing.T) {
	fs := newFakeStore()
	fs.rows = []Row{{ID: 1, AccountID: int64p(1),
		MaxDailyVolume: dec("1000"), MaxOpenOrders: int32p(10),
		DailyWithdrawLimit:  dec("40000"),
		MaxNotionalExposure: dec("10000"), MaxShortExposure: dec("5000")}}
	s := svcWith(t, fs, func() time.Time { return testDay })
	fs.usage[usageKey(1, testDay)] = Usage{
		Volume: decimal.NewFromInt(250), Withdrawn: decimal.NewFromInt(10000)}
	fs.open[1] = 2
	fs.exposures[1] = []SymbolExposure{
		{Symbol: "EURUSD", GrossNotional: decimal.NewFromInt(2500),
			ShortNotional: decimal.NewFromInt(1000)},
	}

	v, err := s.LimitsView(context.Background(), 1, "T1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Day != "2026-09-30" {
		t.Fatalf("Day = %q, want 2026-09-30", v.Day)
	}
	if got := v.DailyVolumePct.String(); got != "25" {
		t.Fatalf("DailyVolumePct = %s, want 25", got)
	}
	if got := v.DailyWithdrawnPct.String(); got != "25" {
		t.Fatalf("DailyWithdrawnPct = %s, want 25", got)
	}
	if got := v.OpenOrdersPct.String(); got != "20" {
		t.Fatalf("OpenOrdersPct = %s, want 20", got)
	}
	if len(v.Symbols) != 1 {
		t.Fatalf("Symbols len = %d, want 1", len(v.Symbols))
	}
	su := v.Symbols[0]
	if got := su.GrossNotionalPct.String(); got != "25" {
		t.Fatalf("GrossNotionalPct = %s, want 25", got)
	}
	if got := su.ShortNotionalPct.String(); got != "20" {
		t.Fatalf("ShortNotionalPct = %s, want 20", got)
	}
}

func TestLimitsViewUnlimitedFieldsNilPct(t *testing.T) {
	fs := newFakeStore() // no rows: qty/volume unlimited, §13.6 exposure defaults
	s := svcWith(t, fs, func() time.Time { return testDay })
	v, err := s.LimitsView(context.Background(), 1, "T1")
	if err != nil {
		t.Fatal(err)
	}
	if v.DailyVolumePct != nil || v.OpenOrdersPct != nil {
		t.Fatal("unset limits must yield nil utilization percentages")
	}
}

// ---------------------------------------------------------------------------
// Migration 047 — MiFID II RTS 9 OTR columns (Task 13.3.6)
// ---------------------------------------------------------------------------

func TestResolveOtrDefaults(t *testing.T) {
	// No rows → the §13.6a canonical pair: 500 events/trade, 60s window.
	lim := Resolve(nil, 7, "T1", "EURUSD")
	if lim.MaxOrderToTradeRatio == nil ||
		!lim.MaxOrderToTradeRatio.Equal(DefaultOtrRatio) {
		t.Fatalf("MaxOrderToTradeRatio = %v, want %s",
			lim.MaxOrderToTradeRatio, DefaultOtrRatio)
	}
	if lim.OtrWindow != DefaultOtrWindow {
		t.Fatalf("OtrWindow = %v, want %v", lim.OtrWindow, DefaultOtrWindow)
	}
}

func TestResolveOtrScopedOverride(t *testing.T) {
	// The §9.6 market-maker allowance: an account(+symbol)-scoped row
	// carrying a higher ratio and a longer window wins over the
	// account-wide row; other symbols keep the account-wide value.
	min := 30 * time.Second
	mm := 5 * time.Minute
	rows := []Row{
		{ID: 1, AccountID: int64p(7),
			MaxOrderToTradeRatio: dec("1000"), OtrWindow: &min},
		{ID: 2, AccountID: int64p(7), Symbol: strp("EURUSD"),
			MaxOrderToTradeRatio: dec("10000"), OtrWindow: &mm},
	}
	lim := Resolve(rows, 7, "T1", "EURUSD")
	if got := lim.MaxOrderToTradeRatio.String(); got != "10000" {
		t.Fatalf("MM row MaxOrderToTradeRatio = %s, want 10000", got)
	}
	if lim.OtrWindow != mm {
		t.Fatalf("MM row OtrWindow = %v, want %v", lim.OtrWindow, mm)
	}
	lim = Resolve(rows, 7, "T1", "GBPUSD")
	if got := lim.MaxOrderToTradeRatio.String(); got != "1000" {
		t.Fatalf("account row MaxOrderToTradeRatio = %s, want 1000", got)
	}
	if lim.OtrWindow != min {
		t.Fatalf("account row OtrWindow = %v, want %v", lim.OtrWindow, min)
	}
	// A row that sets the ratio but not the window inherits the
	// default window.
	rows = []Row{{ID: 3, AccountID: int64p(7), MaxOrderToTradeRatio: dec("50")}}
	lim = Resolve(rows, 7, "T1", "EURUSD")
	if lim.OtrWindow != DefaultOtrWindow {
		t.Fatalf("unset window = %v, want default %v", lim.OtrWindow, DefaultOtrWindow)
	}
}

func TestParseWindow(t *testing.T) {
	w, err := parseWindow(strp("60"))
	if err != nil || *w != 60*time.Second {
		t.Fatalf("60s: %v %v", w, err)
	}
	w, err = parseWindow(strp("0.5"))
	if err != nil || *w != 500*time.Millisecond {
		t.Fatalf("0.5s: %v %v", w, err)
	}
	if _, err := parseWindow(strp("0")); err == nil {
		t.Fatal("zero window must reject")
	}
	if _, err := parseWindow(strp("-30")); err == nil {
		t.Fatal("negative window must reject")
	}
	if _, err := parseWindow(strp("garbage")); err == nil {
		t.Fatal("non-numeric window must reject")
	}
	w, err = parseWindow(nil)
	if w != nil || err != nil {
		t.Fatalf("NULL window → nil: %v %v", w, err)
	}
}

func TestOtrRowJSONRoundTrip(t *testing.T) {
	// The Redis snapshot form: ratio as decimal string, window as a Go
	// duration string — both must survive PublishLimits marshalling.
	win := 90 * time.Second
	r := Row{ID: 9, AccountID: int64p(7),
		MaxOrderToTradeRatio: dec("750"), OtrWindow: &win}
	w := rowJSON{
		ID: r.ID, AccountID: r.AccountID,
		MaxOrderToTradeRatio: decStr(r.MaxOrderToTradeRatio),
		OtrWindow:            durStr(r.OtrWindow),
	}
	blob, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	var back rowJSON
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if back.MaxOrderToTradeRatio == nil || *back.MaxOrderToTradeRatio != "750" {
		t.Fatalf("ratio wire: %v", back.MaxOrderToTradeRatio)
	}
	if back.OtrWindow == nil || *back.OtrWindow != "1m30s" {
		t.Fatalf("window wire: %v", back.OtrWindow)
	}
	if d, err := time.ParseDuration(*back.OtrWindow); err != nil || d != win {
		t.Fatalf("window parse-back: %v %v", d, err)
	}
}
