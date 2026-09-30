// margin_test.go — unit coverage for margin.go (Phase-19 Task 19.3.1;
// spec §13.1/§13.6d): margin-mode parsing + defaults, the mode-switch
// guard, the CROSS/ISOLATED/PORTFOLIO evaluation math, the mark
// fallback chain, and fail-closed currency conversion.
//
// Shared fakes live in sibling files (same package): marginStoreFake,
// markCacheFake, newMarginSvc, d(), requireCode.
package risk

import (
	"context"
	"fmt"
	"testing"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// marginAcctFake extends marginStoreFake (collateral_test.go) with the
// per-account extras the mode/base-currency tests need: a SetMarginMode
// recorder, an open-position-count override, and accounts.base_currency.
type marginAcctFake struct {
	*marginStoreFake
	baseCcy   string
	setModes  []MarginMode
	setErr    error
	openCount int64
}

func (f *marginAcctFake) AccountBaseCurrency(context.Context, int64) (string, error) {
	if f.baseCcy == "" {
		return "USD", nil
	}
	return f.baseCcy, nil
}

func (f *marginAcctFake) SetMarginMode(_ context.Context, _ int64, m MarginMode) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.setModes = append(f.setModes, m)
	if f.account != nil {
		f.account.Mode = m
	}
	return nil
}

func (f *marginAcctFake) OpenPositionCount(context.Context, int64) (int64, error) {
	return f.openCount, nil
}

var _ MarginStore = (*marginAcctFake)(nil)

// ---------------------------------------------------------------------------
// Constructor + mode vocabulary
// ---------------------------------------------------------------------------

func TestMarginServiceNilDeps(t *testing.T) {
	if _, err := NewMarginService(MarginOptions{}); err == nil {
		t.Fatal("nil store must fail construction (fail-closed)")
	}
	if _, err := NewMarginService(MarginOptions{Store: &marginStoreFake{}}); err == nil {
		t.Fatal("nil mark cache must fail construction")
	}
	svc, err := NewMarginService(MarginOptions{
		Store: &marginStoreFake{}, Marks: markCacheFake{}})
	if err != nil {
		t.Fatalf("minimal wiring must succeed: %v", err)
	}
	if svc.corr == nil || svc.now == nil {
		t.Fatal("defaults not installed (corr/now)")
	}
}

func TestMarginModeParse(t *testing.T) {
	for s, want := range map[string]MarginMode{
		"CROSS": ModeCross, "Cross": ModeCross, "cross": ModeCross,
		"ISOLATED": ModeIsolated, "isolated": ModeIsolated,
		"PORTFOLIO": ModePortfolio, "Portfolio": ModePortfolio,
	} {
		got, ok := ParseMarginMode(s)
		if !ok || got != want {
			t.Fatalf("ParseMarginMode(%q) = %q,%v want %q,true", s, got, ok, want)
		}
	}
	for _, bad := range []string{"", "MARGIN", "SPOT", "portfolio ", " cross"} {
		if m, ok := ParseMarginMode(bad); ok {
			t.Fatalf("ParseMarginMode(%q) = %q — must reject", bad, m)
		}
	}
}

func TestMarginDefaultMode(t *testing.T) {
	for cat, want := range map[string]MarginMode{
		"RETAIL":                ModeCross,
		"":                      ModeCross, // unknown → retail default (fail closed)
		"GARBAGE":               ModeCross,
		"PROFESSIONAL":          ModePortfolio,
		"ELIGIBLE_COUNTERPARTY": ModePortfolio,
	} {
		if got := DefaultMode(cat); got != want {
			t.Fatalf("DefaultMode(%q) = %q, want %q", cat, got, want)
		}
	}
}

func TestMarginThresholdsFor(t *testing.T) {
	check := func(cat string, w, c, s string) {
		t.Helper()
		th := ThresholdsFor(cat)
		if !th.Warning.Equal(d(w)) || !th.Call.Equal(d(c)) || !th.StopOut.Equal(d(s)) {
			t.Fatalf("ThresholdsFor(%q) = %s/%s/%s, want %s/%s/%s",
				cat, th.Warning, th.Call, th.StopOut, w, c, s)
		}
	}
	check("RETAIL", "120", "100", "50")                   // ESMA §13.6d
	check("PROFESSIONAL", "100", "80", "30")              // §13.6d pro tier
	check("ELIGIBLE_COUNTERPARTY", "120", "111.1", "100") // institutional fallback
	check("", "120", "100", "50")                         // unknown → retail (fail closed)
}

// ---------------------------------------------------------------------------
// ModeFor / SetMode
// ---------------------------------------------------------------------------

func TestMarginServiceModeFor(t *testing.T) {
	store := &marginStoreFake{
		account:  &MarginAccount{AccountID: 7, Mode: ModeIsolated},
		category: CategoryRetail,
	}
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: markCacheFake{}})
	ctx := context.Background()

	if m, err := svc.ModeFor(ctx, 7); err != nil || m != ModeIsolated {
		t.Fatalf("row mode: %q %v, want ISOLATED", m, err)
	}
	// No margin_accounts row → §13.1 category default.
	store.account = nil
	store.category = CategoryProfessional
	if m, err := svc.ModeFor(ctx, 7); err != nil || m != ModePortfolio {
		t.Fatalf("professional default: %q %v, want PORTFOLIO", m, err)
	}
	store.category = CategoryRetail
	if m, err := svc.ModeFor(ctx, 7); err != nil || m != ModeCross {
		t.Fatalf("retail default: %q %v, want CROSS", m, err)
	}
	// Store failure propagates wrapped (fail closed).
	store.getErr = fmt.Errorf("pg down")
	if _, err := svc.ModeFor(ctx, 7); err == nil {
		t.Fatal("store error must propagate")
	}
}

func TestMarginServiceModeForErrorCode(t *testing.T) {
	store := &marginStoreFake{getErr: fmt.Errorf("pg down")}
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: markCacheFake{}})
	_, err := svc.ModeFor(context.Background(), 7)
	requireCode(t, err, CodeRiskLimitsInternal)
}

func TestMarginServiceSetMode(t *testing.T) {
	ctx := context.Background()
	store := &marginAcctFake{marginStoreFake: &marginStoreFake{
		account:  &MarginAccount{AccountID: 7, Mode: ModeCross},
		category: CategoryRetail,
	}}
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: markCacheFake{}})

	// Unknown mode → INVALID_REQUEST, store untouched.
	if _, err := svc.SetMode(ctx, 7, "margin"); err == nil {
		t.Fatal("invalid mode must be rejected")
	} else {
		requireCode(t, err, CodeInvalidRequest)
	}
	if len(store.setModes) != 0 {
		t.Fatal("invalid mode must not reach the store")
	}

	// Idempotent no-op: same mode returns early — open positions must
	// not matter and no write lands.
	store.openCount = 3
	res, err := svc.SetMode(ctx, 7, "cross")
	if err != nil || res.Mode != ModeCross {
		t.Fatalf("idempotent same-mode: %+v %v", res, err)
	}
	if len(store.setModes) != 0 {
		t.Fatalf("same-mode switch must not write, got %v", store.setModes)
	}

	// Open positions block the switch (Task 19.3.1 DoD, §23 409).
	if _, err := svc.SetMode(ctx, 7, "isolated"); err == nil {
		t.Fatal("open positions must block the switch")
	} else {
		requireCode(t, err, CodeMarginModeBlocked)
	}
	if len(store.setModes) != 0 {
		t.Fatal("blocked switch must not write")
	}

	// Flat account switches freely; case-insensitive token.
	store.openCount = 0
	res, err = svc.SetMode(ctx, 7, "PORTFOLIO")
	if err != nil || res.Mode != ModePortfolio {
		t.Fatalf("switch: %+v %v", res, err)
	}
	if len(store.setModes) != 1 || store.setModes[0] != ModePortfolio {
		t.Fatalf("recorded modes: %v", store.setModes)
	}
	// Row now reports PORTFOLIO — switching to it again is a no-op.
	if _, err := svc.SetMode(ctx, 7, "portfolio"); err != nil {
		t.Fatalf("second same-mode: %v", err)
	}
	if len(store.setModes) != 1 {
		t.Fatal("idempotent re-switch must not write")
	}

	// Store write failure propagates wrapped.
	store.setErr = fmt.Errorf("pg down")
	if _, err := svc.SetMode(ctx, 7, "isolated"); err == nil {
		t.Fatal("store failure must propagate")
	} else {
		requireCode(t, err, CodeRiskLimitsInternal)
	}
}

// ---------------------------------------------------------------------------
// Evaluation — CROSS
// ---------------------------------------------------------------------------

// marginEvalFixture seeds one CROSS retail account: USD balance
// 2000+500 locked, one EUR/USD LONG 10000 @ 1.10 with margin_used 1000.
func marginEvalFixture() (*marginStoreFake, markCacheFake) {
	store := &marginStoreFake{
		account:  &MarginAccount{AccountID: 7, Mode: ModeCross, Status: "NORMAL"},
		category: CategoryRetail,
		balances: []BalanceAmount{
			{Currency: "USD", Available: d("2000"), Locked: d("500")},
		},
		positions: []MarginPosition{{
			ID: 11, InstrumentID: 1, Symbol: "EUR/USD", Side: "LONG",
			Quantity: d("10000"), EntryPrice: d("1.10"),
			MarginUsed: d("1000"), QuoteCurrency: "USD", BaseCurrency: "EUR",
			MaxLeverage: 30,
		}},
	}
	return store, markCacheFake{m: map[string]decimal.Decimal{"EUR/USD": d("1.20")}}
}

func TestMarginServiceEvaluateCross(t *testing.T) {
	store, marks := marginEvalFixture()
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	// uPnL = 10000 × (1.20 − 1.10) = +1000 → equity 3500, used 1000,
	// level 350% → NORMAL; available = 2500.
	if !snap.Equity.Equal(d("3500")) {
		t.Fatalf("equity = %s, want 3500", snap.Equity)
	}
	if !snap.UsedMargin.Equal(d("1000")) {
		t.Fatalf("used = %s, want 1000", snap.UsedMargin)
	}
	if !snap.AvailableMargin.Equal(d("2500")) {
		t.Fatalf("available = %s, want 2500", snap.AvailableMargin)
	}
	if snap.LevelPct == nil || !snap.LevelPct.Equal(d("350")) {
		t.Fatalf("level = %v, want 350", snap.LevelPct)
	}
	if snap.Status != "NORMAL" || snap.Mode != ModeCross || snap.ClientCategory != "RETAIL" {
		t.Fatalf("header: %+v", snap)
	}
	if len(snap.Positions) != 1 {
		t.Fatalf("position evals: %+v", snap.Positions)
	}
	pe := snap.Positions[0]
	if pe.PositionID != 11 || pe.MarkSource != MarkSourceOracle ||
		pe.Mark != "1.2" || pe.UnrealizedUSD != "1000" || pe.MarginUSD != "1000" ||
		pe.Side != "LONG" || pe.Quantity != "10000" {
		t.Fatalf("position eval: %+v", pe)
	}
	if len(snap.Unvalued) != 0 {
		t.Fatalf("unvalued: %v", snap.Unvalued)
	}
}

func TestMarginServiceEvaluateMarkFallbackChain(t *testing.T) {
	ctx := context.Background()
	store, _ := marginEvalFixture()

	// Oracle silent → stored mark_price wins.
	stored := d("1.20")
	store.positions[0].StoredMark = &stored
	svc := newMarginSvc(t, MarginOptions{
		Store: store, Marks: markCacheFake{m: map[string]decimal.Decimal{}}})
	snap, err := svc.Evaluate(ctx, 7)
	if err != nil {
		t.Fatalf("stored-mark eval: %v", err)
	}
	if !snap.Equity.Equal(d("3500")) || snap.Positions[0].MarkSource != MarkSourceStoredMark {
		t.Fatalf("stored fallback: equity=%s src=%s", snap.Equity, snap.Positions[0].MarkSource)
	}

	// Stored absent too → entry price, zero uPnL.
	store.positions[0].StoredMark = nil
	snap, err = svc.Evaluate(ctx, 7)
	if err != nil {
		t.Fatalf("entry-mark eval: %v", err)
	}
	if !snap.Equity.Equal(d("2500")) || snap.Positions[0].MarkSource != MarkSourceEntry ||
		snap.Positions[0].UnrealizedUSD != "0" {
		t.Fatalf("entry fallback: %+v", snap.Positions[0])
	}
	if snap.LevelPct == nil || !snap.LevelPct.Equal(d("250")) {
		t.Fatalf("entry fallback level: %v", snap.LevelPct)
	}

	// A non-positive oracle mark is not a usable mark — entry wins.
	svc2 := newMarginSvc(t, MarginOptions{
		Store: store,
		Marks: markCacheFake{m: map[string]decimal.Decimal{"EUR/USD": d("-1")}}})
	snap, err = svc2.Evaluate(ctx, 7)
	if err != nil {
		t.Fatalf("non-positive mark eval: %v", err)
	}
	if snap.Positions[0].MarkSource != MarkSourceEntry {
		t.Fatalf("non-positive oracle must fall back, got %s", snap.Positions[0].MarkSource)
	}
}

func TestMarginServiceEvaluateShortInverseRate(t *testing.T) {
	// USD/JPY SHORT — JPY-quoted P&L and margin convert through the
	// USD/JPY inverse pair (rate = 1/mark).
	store := &marginStoreFake{
		account:  &MarginAccount{AccountID: 7, Mode: ModeCross},
		category: CategoryRetail,
		balances: []BalanceAmount{{Currency: "USD", Available: d("1000")}},
		positions: []MarginPosition{{
			ID: 21, InstrumentID: 3, Symbol: "USD/JPY", Side: "SHORT",
			Quantity: d("10000"), EntryPrice: d("150"),
			MarginUsed:    d("50000"), // JPY
			QuoteCurrency: "JPY", BaseCurrency: "USD", MaxLeverage: 30,
		}},
		pairs: map[string]FxPair{"JPY": {Symbol: "USD/JPY", Inverted: true}},
	}
	marks := markCacheFake{m: map[string]decimal.Decimal{"USD/JPY": d("149")}}
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	// SHORT won: upnl = −10000 × (149−150) = +10000 JPY → ×(1/149) USD.
	rate := decimal.NewFromInt(1).Div(d("149"))
	wantUpnl := d("10000").Mul(rate).Round(8)
	wantUsed := d("50000").Mul(rate).Round(8)
	if !decimal.RequireFromString(snap.Positions[0].UnrealizedUSD).Equal(wantUpnl) {
		t.Fatalf("upnl USD = %s, want %s", snap.Positions[0].UnrealizedUSD, wantUpnl)
	}
	if !snap.UsedMargin.Equal(wantUsed) {
		t.Fatalf("used = %s, want %s (50000 JPY / 149)", snap.UsedMargin, wantUsed)
	}
	wantEquity := d("1000").Add(wantUpnl)
	if !snap.Equity.Equal(wantEquity) {
		t.Fatalf("equity = %s, want %s", snap.Equity, wantEquity)
	}
	if snap.Status != "NORMAL" {
		t.Fatalf("status %s", snap.Status)
	}
}

func TestMarginServiceEvaluateMissingQuoteRateFailsClosed(t *testing.T) {
	// §2.7: a required-margin leg that cannot be priced in USD aborts the
	// whole evaluation — never silently understated.
	store := &marginStoreFake{
		account:  &MarginAccount{AccountID: 7, Mode: ModeCross},
		category: CategoryRetail,
		balances: []BalanceAmount{{Currency: "USD", Available: d("10000")}},
		positions: []MarginPosition{{
			ID: 31, InstrumentID: 9, Symbol: "EUR/CHF", Side: "LONG",
			Quantity: d("10000"), EntryPrice: d("1.00"),
			MarginUsed: d("500"), QuoteCurrency: "CHF", BaseCurrency: "EUR",
			MaxLeverage: 30,
		}},
		pairs: map[string]FxPair{}, // no CHF coverage
	}
	marks := markCacheFake{m: map[string]decimal.Decimal{"EUR/CHF": d("1.00")}}
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err == nil || snap != nil {
		t.Fatalf("missing USD rate must fail closed, got %+v %v", snap, err)
	}
	requireCode(t, err, CodeOracleUnavailable)
}

func TestMarginServiceEvaluateUnvaluedBalanceDisclosed(t *testing.T) {
	// Balance legs in unconvertible currencies contribute 0 (conservative
	// under-valuation) and are disclosed — never silently dropped.
	store := &marginStoreFake{
		account:  &MarginAccount{AccountID: 7, Mode: ModeCross},
		category: CategoryRetail,
		balances: []BalanceAmount{
			{Currency: "USD", Available: d("1000")},
			{Currency: "ZZZ", Available: d("500")}, // no {ZZZ}/USD pair
		},
	}
	svc := newMarginSvc(t, MarginOptions{
		Store: store, Marks: markCacheFake{m: map[string]decimal.Decimal{}}})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !snap.Equity.Equal(d("1000")) {
		t.Fatalf("equity = %s, want 1000 (ZZZ excluded)", snap.Equity)
	}
	if len(snap.Unvalued) != 1 || snap.Unvalued[0] != "ZZZ" {
		t.Fatalf("unvalued = %v, want [ZZZ]", snap.Unvalued)
	}
	// No used margin → infinite level, NORMAL.
	if snap.LevelPct != nil || snap.Status != "NORMAL" {
		t.Fatalf("zero-used snapshot: level=%v status=%s", snap.LevelPct, snap.Status)
	}
}

func TestMarginServiceEvaluateStoreAndMarkErrors(t *testing.T) {
	ctx := context.Background()
	store, _ := marginEvalFixture()
	store.getErr = fmt.Errorf("pg down")
	svc := newMarginSvc(t, MarginOptions{
		Store: store, Marks: markCacheFake{m: map[string]decimal.Decimal{}}})
	if _, err := svc.Evaluate(ctx, 7); err == nil {
		t.Fatal("store failure must propagate")
	} else {
		requireCode(t, err, CodeRiskLimitsInternal)
	}

	store, _ = marginEvalFixture()
	svc = newMarginSvc(t, MarginOptions{
		Store: store, Marks: markCacheFake{err: fmt.Errorf("redis down")}})
	if _, err := svc.Evaluate(ctx, 7); err == nil {
		t.Fatal("mark batch failure must fail closed")
	} else {
		requireCode(t, err, CodeOracleUnavailable)
	}
}

// ---------------------------------------------------------------------------
// Status machine + ISOLATED aggregation
// ---------------------------------------------------------------------------

// marginStatusStore builds a CROSS account whose level is equity/used
// exactly (mark == entry ⇒ zero uPnL).
func marginStatusStore(mode MarginMode, equity, used string) *marginStoreFake {
	return &marginStoreFake{
		account:  &MarginAccount{AccountID: 7, Mode: mode},
		category: CategoryRetail,
		balances: []BalanceAmount{{Currency: "USD", Available: d(equity)}},
		positions: []MarginPosition{{
			ID: 1, Symbol: "EUR/USD", Side: "LONG", Quantity: d("1"),
			EntryPrice: d("1"), MarginUsed: d(used),
			QuoteCurrency: "USD", BaseCurrency: "EUR", MaxLeverage: 30,
			IsolatedAllocated: d(used),
		}},
	}
}

func TestMarginServiceEvaluateStatusMachine(t *testing.T) {
	ctx := context.Background()
	marks := markCacheFake{m: map[string]decimal.Decimal{"EUR/USD": d("1")}}
	for _, tc := range []struct {
		equity, used, want string
	}{
		{"40", "100", "LIQUIDATING"}, // ≤ 50 retail stop-out
		{"50", "100", "LIQUIDATING"}, // stop-out boundary is inclusive
		{"50.01", "100", "MARGIN_CALL"},
		{"111.1", "100", "MARGIN_CALL"}, // §13.3 canonical trigger inclusive
		{"111.11", "100", "NORMAL"},
		{"350", "100", "NORMAL"},
	} {
		svc := newMarginSvc(t, MarginOptions{
			Store: marginStatusStore(ModeCross, tc.equity, tc.used), Marks: marks})
		snap, err := svc.Evaluate(ctx, 7)
		if err != nil {
			t.Fatalf("evaluate %s/%s: %v", tc.equity, tc.used, err)
		}
		if snap.Status != tc.want {
			t.Fatalf("level %s%%: status %s, want %s",
				snap.LevelPct, snap.Status, tc.want)
		}
	}
}

func TestMarginServiceEvaluateIsolatedAggregatesAndCaps(t *testing.T) {
	ctx := context.Background()
	mark := d("1.10")
	store := &marginStoreFake{
		account:  &MarginAccount{AccountID: 7, Mode: ModeIsolated},
		category: CategoryRetail,
		balances: []BalanceAmount{{Currency: "USD", Available: d("40")}},
		positions: []MarginPosition{
			{ID: 1, Symbol: "EUR/USD", Side: "LONG", Quantity: d("10000"),
				EntryPrice: d("1.10"), StoredMark: &mark, MarginUsed: d("1000"),
				QuoteCurrency: "USD", BaseCurrency: "EUR", MaxLeverage: 30,
				IsolatedAllocated: d("100")},
			{ID: 2, Symbol: "GBP/USD", Side: "LONG", Quantity: d("5000"),
				EntryPrice: d("1.25"), MarginUsed: d("500"),
				QuoteCurrency: "USD", BaseCurrency: "GBP", MaxLeverage: 30,
				IsolatedAllocated: d("250")},
		},
	}
	marks := markCacheFake{m: map[string]decimal.Decimal{
		"EUR/USD": d("1.10"), "GBP/USD": d("1.25")}}
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks})
	snap, err := svc.Evaluate(ctx, 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	// ISOLATED used margin is Σ allocated collateral (100+250), NOT the
	// positions' margin_used legs.
	if !snap.UsedMargin.Equal(d("350")) {
		t.Fatalf("isolated used = %s, want 350", snap.UsedMargin)
	}
	// Equity 40 (flat marks) → level ≈11.43% — at-or-below stop-out for a
	// cross account, but ISOLATED caps the account-level status at
	// MARGIN_CALL (per-position liquidation is the isolated engine's job).
	if snap.Status != "MARGIN_CALL" {
		t.Fatalf("isolated status = %s, want MARGIN_CALL (cap, never LIQUIDATING)",
			snap.Status)
	}
}

func TestMarginServiceEvaluateIsolatedNonUSDBase(t *testing.T) {
	// Allocated collateral is base-currency denominated (migration 106) —
	// a EUR-base account converts via EUR/USD.
	store := &marginAcctFake{marginStoreFake: &marginStoreFake{
		account:  &MarginAccount{AccountID: 7, Mode: ModeIsolated},
		category: CategoryRetail,
		balances: []BalanceAmount{
			{Currency: "USD", Available: d("10000")},
			{Currency: "EUR", Available: d("0")}, // makes EUR a requested ccy
		},
		positions: []MarginPosition{{
			ID: 1, Symbol: "EUR/USD", Side: "LONG", Quantity: d("1"),
			EntryPrice: d("1.10"), MarginUsed: d("0"),
			QuoteCurrency: "USD", BaseCurrency: "EUR", MaxLeverage: 30,
			IsolatedAllocated: d("500"), // EUR
		}},
		pairs: map[string]FxPair{"EUR": {Symbol: "EUR/USD"}},
	}, baseCcy: "EUR"}
	marks := markCacheFake{m: map[string]decimal.Decimal{"EUR/USD": d("1.10")}}
	svc := newMarginSvc(t, MarginOptions{Store: store, Marks: marks})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	// 500 EUR × 1.10 = 550 USD.
	if !snap.UsedMargin.Equal(d("550")) {
		t.Fatalf("used = %s, want 550", snap.UsedMargin)
	}
}

func TestMarginServiceEvaluateIsolatedMissingBaseRateFailsClosed(t *testing.T) {
	// EUR-base account with NO EUR balance/quote leg and no pair → the
	// base-currency conversion cannot resolve → PRICE_ORACLE_UNAVAILABLE.
	store := &marginAcctFake{marginStoreFake: &marginStoreFake{
		account:  &MarginAccount{AccountID: 7, Mode: ModeIsolated},
		category: CategoryRetail,
		balances: []BalanceAmount{{Currency: "USD", Available: d("10000")}},
		positions: []MarginPosition{{
			ID: 1, Symbol: "EUR/USD", Side: "LONG", Quantity: d("1"),
			EntryPrice: d("1.10"), QuoteCurrency: "USD", BaseCurrency: "EUR",
			MaxLeverage: 30, IsolatedAllocated: d("500"),
		}},
		pairs: map[string]FxPair{},
	}, baseCcy: "EUR"}
	svc := newMarginSvc(t, MarginOptions{
		Store: store, Marks: markCacheFake{m: map[string]decimal.Decimal{"EUR/USD": d("1.10")}}})
	if _, err := svc.Evaluate(context.Background(), 7); err == nil {
		t.Fatal("unpriced base currency must fail closed")
	} else {
		requireCode(t, err, CodeOracleUnavailable)
	}
}

// ---------------------------------------------------------------------------
// Derived required margin (margin_used = 0 → notional/leverage)
// ---------------------------------------------------------------------------

func TestMarginServiceEvaluateDerivedMargin(t *testing.T) {
	ctx := context.Background()
	marks := markCacheFake{m: map[string]decimal.Decimal{"EUR/USD": d("1.20")}}
	mk := func(lev int64) *marginStoreFake {
		return &marginStoreFake{
			account:  &MarginAccount{AccountID: 7, Mode: ModeCross},
			category: CategoryRetail,
			balances: []BalanceAmount{{Currency: "USD", Available: d("100000")}},
			positions: []MarginPosition{{
				ID: 1, Symbol: "EUR/USD", Side: "LONG", Quantity: d("10000"),
				EntryPrice: d("1.10"), MarginUsed: decimal.Zero,
				QuoteCurrency: "USD", BaseCurrency: "EUR", MaxLeverage: lev,
			}},
		}
	}
	// margin_used = 0 → derive notional/leverage: 10000 × 1.20 / 30 = 400.
	snap, err := newMarginSvc(t, MarginOptions{Store: mk(30), Marks: marks}).
		Evaluate(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UsedMargin.Equal(d("400")) {
		t.Fatalf("derived used = %s, want 400", snap.UsedMargin)
	}
	// lev ≤ 0 → treated as 1:1 (full notional 12000).
	snap, err = newMarginSvc(t, MarginOptions{Store: mk(0), Marks: marks}).
		Evaluate(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UsedMargin.Equal(d("12000")) {
		t.Fatalf("1:1 derived used = %s, want 12000", snap.UsedMargin)
	}
}

// ---------------------------------------------------------------------------
// portfolioMargin — §13.6g gate/offset/floor/cap on the pure function
// (service-level correlation coverage lives in correlation_offset_test.go)
// ---------------------------------------------------------------------------

func mgnPosEval(symbol, side, marginUSD string) posEval {
	return posEval{
		p:    MarginPosition{Symbol: symbol, Side: side},
		mark: d("1"), src: MarkSourceOracle,
		upnlUSD:   decimal.Zero,
		marginUSD: d(marginUSD),
	}
}

func TestMarginPortfolioMarginMath(t *testing.T) {
	// Degenerate inputs → gross, no pair scan.
	if got := portfolioMargin(nil, NoCorrelation{}); !got.IsZero() {
		t.Fatalf("empty portfolio margin = %s, want 0", got)
	}
	if got := portfolioMargin(
		[]posEval{mgnPosEval("EUR/USD", "LONG", "1000")}, NoCorrelation{}); !got.Equal(d("1000")) {
		t.Fatalf("single-position margin = %s, want 1000 (gross)", got)
	}

	two := []posEval{
		mgnPosEval("EUR/USD", "LONG", "1000"),
		mgnPosEval("GBP/USD", "LONG", "1000"),
	}
	// No estimate → treated as ρ=0 → no offset.
	if got := portfolioMargin(two, CorrelationFunc(func(string, string) (float64, bool) {
		return 0, false
	})); !got.Equal(d("2000")) {
		t.Fatalf("no-estimate margin = %s, want 2000", got)
	}
	// Inside the gate (|ρ| < 0.70 → ρ_pos > −7000 bps) → no offset.
	if got := portfolioMargin(two, CorrelationFunc(func(string, string) (float64, bool) {
		return -6999, true
	})); !got.Equal(d("2000")) {
		t.Fatalf("inside-gate margin = %s, want 2000", got)
	}
	// ρ = −0.70 exactly sits ON the gate (rhoPos > −7000 fails) → hedge
	// credit applies: 1000 × 0.70 × 0.5 = 350 → net 1650.
	if got := portfolioMargin(two, CorrelationFunc(func(string, string) (float64, bool) {
		return -7000, true
	})); !got.Equal(d("1650")) {
		t.Fatalf("gate boundary margin = %s, want 1650", got)
	}
	// ρ = −0.75 past the gate, same-side positions, default factor 0.5:
	// credit = min(1000,1000) × 0.75 × 0.5 = 375 → net 1625.
	if got := portfolioMargin(two, CorrelationFunc(func(string, string) (float64, bool) {
		return -7500, true
	})); !got.Equal(d("1625")) {
		t.Fatalf("offset margin = %s, want 1625", got)
	}
	// Opposite sides flip the sign: instrument ρ +0.90 on LONG×SHORT →
	// position ρ −0.90 → hedge credit 450.
	hedge := []posEval{
		mgnPosEval("EUR/USD", "LONG", "1000"),
		mgnPosEval("GBP/USD", "SHORT", "1000"),
	}
	if got := portfolioMargin(hedge, CorrelationFunc(func(string, string) (float64, bool) {
		return 9000, true
	})); !got.Equal(d("1550")) {
		t.Fatalf("opposite-side hedge margin = %s, want 1550", got)
	}
	// Same sides with instrument ρ +0.90 → position ρ +0.90 → no hedge.
	if got := portfolioMargin(two, CorrelationFunc(func(string, string) (float64, bool) {
		return 9000, true
	})); !got.Equal(d("2000")) {
		t.Fatalf("same-side positive ρ margin = %s, want 2000", got)
	}
	// min() picks the smaller leg: margins 1000/200 → credit 200×0.9×0.5=90.
	uneven := []posEval{
		mgnPosEval("EUR/USD", "LONG", "1000"),
		mgnPosEval("GBP/USD", "LONG", "200"),
	}
	if got := portfolioMargin(uneven, CorrelationFunc(func(string, string) (float64, bool) {
		return -9000, true
	})); !got.Equal(d("1110")) {
		t.Fatalf("min-leg margin = %s, want 1110", got)
	}
}

// ---------------------------------------------------------------------------
// Small value-type coverage
// ---------------------------------------------------------------------------

func TestMarginBalanceTotal(t *testing.T) {
	b := BalanceAmount{Currency: "USD", Available: d("3"), Locked: d("4")}
	if !b.Total().Equal(d("7")) {
		t.Fatalf("total = %s, want 7", b.Total())
	}
}
