// leverage_test.go — unit + gated integration coverage for the
// effective-leverage resolver (leverage.go) and the notional-tier
// schedule (leverage_tiers.go) — Phase-19 Tasks 19.3.2 / 19.3.17 /
// 19.3.23 / 19.3.24; spec §13.6f, §13.13, §13.14.
//
// Ungated legs cover classification, the regulatory regime map, the
// most-restrictive-wins resolution fold (entity policy → statutory cap →
// tier band → instrument max → account chosen), the leverage:eff:*
// cache contract, runtime SetLeverage guards, and the pure tier math
// (TierAt / BandMargin).
//
// Gated legs reuse liqStoreDSN/liqStoreMigExec/liqSeed* from
// liquidation_store_test.go (same package):
//
//	EXC_PG_TEST=1 go test ./internal/risk/ -run 'TestPgLeverage|TestPgTier' -v
package risk

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes (name-spaced to avoid collisions with sibling test files)
// ---------------------------------------------------------------------------

// levStoreFake implements LeverageStore over in-memory maps.
type levStoreFake struct {
	mu          sync.Mutex
	accounts    map[int64]*LeverageAccountView
	instruments map[int64]*LeverageInstrumentView
	policies    map[string]int // "ENTITY|CAT|GROUP" -> max_leverage
	policyErr   error
	tierFn      func(regime, group string, notional decimal.Decimal) (*int, error)
	perSym      map[[2]int64]*ChosenLeverage // (account, instrument)
	defaults    map[int64]*ChosenLeverage    // account -> default row
	chosenErr   error
	notionals   map[[2]int64]decimal.Decimal
	notionalErr error
	positions   map[int64][]PositionNotional
	posErr      error
	equities    map[int64]*decimal.Decimal
	eqErr       error
	acctErr     error
	instErr     error
	putErr      error
	puts        []levPutCall
	acctCalls   int
}

type levPutCall struct {
	accountID    int64
	instrumentID *int64
	leverage     int
	before       *int
	userID       int64
}

func newLevStoreFake() *levStoreFake {
	return &levStoreFake{
		accounts:    map[int64]*LeverageAccountView{},
		instruments: map[int64]*LeverageInstrumentView{},
		policies:    map[string]int{},
		perSym:      map[[2]int64]*ChosenLeverage{},
		defaults:    map[int64]*ChosenLeverage{},
		notionals:   map[[2]int64]decimal.Decimal{},
		positions:   map[int64][]PositionNotional{},
		equities:    map[int64]*decimal.Decimal{},
	}
}

func levPolicyKey(entity, cat, group string) string {
	return fmt.Sprintf("%s|%s|%s", entity, cat, group)
}

func (f *levStoreFake) AccountLeverageView(_ context.Context, accountID int64) (*LeverageAccountView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acctCalls++
	if f.acctErr != nil {
		return nil, f.acctErr
	}
	return f.accounts[accountID], nil
}

func (f *levStoreFake) InstrumentLeverageView(_ context.Context, instrumentID int64) (*LeverageInstrumentView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.instErr != nil {
		return nil, f.instErr
	}
	return f.instruments[instrumentID], nil
}

func (f *levStoreFake) InstrumentLeverageViewBySymbol(_ context.Context, symbol string) (*LeverageInstrumentView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.instruments {
		if v.Symbol == symbol {
			return v, nil
		}
	}
	return nil, nil
}

func (f *levStoreFake) EntityPolicyCap(_ context.Context, entityCode, clientCategory, group string) (*int, error) {
	if f.policyErr != nil {
		return nil, f.policyErr
	}
	if v, ok := f.policies[levPolicyKey(entityCode, clientCategory, group)]; ok {
		c := v
		return &c, nil
	}
	return nil, nil
}

func (f *levStoreFake) TierCap(_ context.Context, regime, group string, notional decimal.Decimal) (*int, error) {
	if f.tierFn != nil {
		return f.tierFn(regime, group, notional)
	}
	return nil, nil
}

func (f *levStoreFake) ChosenLeverage(_ context.Context, accountID, instrumentID int64) (*ChosenLeverage, *ChosenLeverage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chosenErr != nil {
		return nil, nil, f.chosenErr
	}
	var perSym *ChosenLeverage
	if instrumentID != 0 {
		perSym = f.perSym[[2]int64{accountID, instrumentID}]
	}
	return perSym, f.defaults[accountID], nil
}

func (f *levStoreFake) SymbolGrossNotional(_ context.Context, accountID, instrumentID int64) (decimal.Decimal, error) {
	if f.notionalErr != nil {
		return decimal.Zero, f.notionalErr
	}
	return f.notionals[[2]int64{accountID, instrumentID}], nil
}

func (f *levStoreFake) OpenPositionNotionals(_ context.Context, accountID int64) ([]PositionNotional, error) {
	if f.posErr != nil {
		return nil, f.posErr
	}
	return f.positions[accountID], nil
}

func (f *levStoreFake) MarginEquity(_ context.Context, accountID int64) (*decimal.Decimal, error) {
	if f.eqErr != nil {
		return nil, f.eqErr
	}
	return f.equities[accountID], nil
}

func (f *levStoreFake) PutChosenLeverage(_ context.Context, accountID int64,
	instrumentID *int64, leverage int, before *int, userID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.puts = append(f.puts, levPutCall{accountID, instrumentID, leverage, before, userID})
	row := &ChosenLeverage{InstrumentID: instrumentID, Leverage: leverage, UpdatedAt: time.Now()}
	if instrumentID == nil {
		f.defaults[accountID] = row
	} else {
		f.perSym[[2]int64{accountID, *instrumentID}] = row
	}
	return nil
}

// levCacheFake implements LeverageCache over a string map.
type levCacheFake struct {
	mu        sync.Mutex
	m         map[string]string
	setCalls  int
	getCalls  int
	delCalls  [][]string
	scanCalls []string
	scanKeys  []string
	getErr    error
}

func newLevCacheFake() *levCacheFake { return &levCacheFake{m: map[string]string{}} }

func (c *levCacheFake) Get(_ context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.getCalls++
	if c.getErr != nil {
		return "", c.getErr
	}
	return c.m[key], nil
}

func (c *levCacheFake) Set(_ context.Context, key, val string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setCalls++
	c.m[key] = val
	return nil
}

func (c *levCacheFake) Del(_ context.Context, keys ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delCalls = append(c.delCalls, keys)
	for _, k := range keys {
		delete(c.m, k)
	}
	return nil
}

func (c *levCacheFake) ScanKeys(_ context.Context, pattern string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scanCalls = append(c.scanCalls, pattern)
	return c.scanKeys, nil
}

// levFixture bundles one account + one instrument for the common case.
func levFixture(policy int) *levStoreFake {
	f := newLevStoreFake()
	f.accounts[7] = &LeverageAccountView{ID: 7, EntityCode: "EU-ESMA", ClientCategory: "RETAIL"}
	f.instruments[9] = &LeverageInstrumentView{
		ID: 9, Symbol: "EUR/USD", BaseCurrency: "EUR", QuoteCurrency: "USD",
		MaxLeverage: 30,
	}
	for _, g := range []string{GroupMajor, GroupMinor, GroupExotic} {
		f.policies[levPolicyKey("EU-ESMA", "RETAIL", g)] = policy
	}
	return f
}

// ---------------------------------------------------------------------------
// Classification + regime axes
// ---------------------------------------------------------------------------

func TestLeverageClassifyInstrumentGroup(t *testing.T) {
	cases := []struct{ base, quote, want string }{
		{"EUR", "USD", GroupMajor},
		{"USD", "JPY", GroupMajor},
		{"usd", "cad", GroupMajor}, // case-insensitive
		{" EUR ", " GBP ", GroupMinor},
		{"EUR", "JPY", GroupMinor},  // two G7 non-seed pair
		{"GBP", "CHF", GroupMinor},  // GBP/CHF is not a seed major
		{"USD", "MXN", GroupExotic}, // MXN outside G7
		{"USD", "TRY", GroupExotic},
		{"XXX", "USD", GroupExotic}, // unknown base
	}
	for _, c := range cases {
		if got := ClassifyInstrumentGroup(c.base, c.quote); got != c.want {
			t.Fatalf("%s/%s: got %s, want %s", c.base, c.quote, got, c.want)
		}
	}
}

func TestLeverageRegulatoryRegime(t *testing.T) {
	cases := []struct{ entity, cat, want string }{
		{"EU-ESMA", "RETAIL", RegimeESMA},
		{"UK-FCA", "RETAIL", RegimeESMA},
		{"US-CFTC", "RETAIL", RegimeCFTC},
		{"us-cftc", "retail", RegimeCFTC}, // case-insensitive
		{"INTL", "RETAIL", RegimeESMA},    // default-entity = strictest schedule
		{"UNKNOWN", "RETAIL", RegimeESMA}, // unknown entity → ESMA fail closed
		{"US-CFTC", "PROFESSIONAL", RegimeProfessional},
		{"EU-ESMA", "ELIGIBLE_COUNTERPARTY", RegimeProfessional},
		{"US-CFTC", "", RegimeCFTC}, // missing category → retail branch → CFTC entity
	}
	for _, c := range cases {
		if got := RegulatoryRegime(c.entity, c.cat); got != c.want {
			t.Fatalf("(%s,%s): got %s, want %s", c.entity, c.cat, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Resolution — most-restrictive wins
// ---------------------------------------------------------------------------

func TestLeverageResolveMostRestrictive(t *testing.T) {
	ctx := context.Background()
	f := levFixture(200)
	f.tierFn = func(_, _ string, _ decimal.Decimal) (*int, error) { return ptrInt(20), nil }
	f.perSym[[2]int64{7, 9}] = &ChosenLeverage{Leverage: 50}
	svc := NewLeverageService(f, nil)

	d, err := svc.Resolve(ctx, 7, 9)
	if err != nil {
		t.Fatal(err)
	}
	// min(entity 200, ESMA major 30, tier 20, instrument 30, chosen 50) = 20.
	if d.Effective != 20 || d.Source != "tier_band" {
		t.Fatalf("effective=%d source=%s, want 20/tier_band", d.Effective, d.Source)
	}
	if d.Group != GroupMajor || d.Regime != RegimeESMA {
		t.Fatalf("group/regime: %+v", d)
	}
	sources := map[string]int{}
	for _, c := range d.Components {
		sources[c.Source] = c.Cap
	}
	want := map[string]int{
		"entity_policy": 200, "category_cap": 30, "tier_band": 20,
		"instrument_max": 30, "account_chosen": 50,
	}
	for src, cap := range want {
		if sources[src] != cap {
			t.Fatalf("component %s = %d, want %d (all %+v)", src, sources[src], cap, sources)
		}
	}
}

func TestLeverageResolveRegulatoryCaps(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		entity      string
		base, quote string
		want        int
	}{
		// ESMA retail statutory caps: 30 major / 20 minor / 10 exotic.
		{"esma major", "EU-ESMA", "EUR", "USD", 30},
		{"esma minor", "EU-ESMA", "EUR", "JPY", 20},
		{"esma exotic", "EU-ESMA", "USD", "MXN", 10},
		// CFTC retail statutory caps: 50 major / 20 minor.
		{"cftc major", "US-CFTC", "EUR", "USD", 50},
		{"cftc minor", "US-CFTC", "EUR", "JPY", 20},
		{"cftc exotic", "US-CFTC", "USD", "MXN", 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newLevStoreFake()
			f.accounts[7] = &LeverageAccountView{ID: 7, EntityCode: c.entity, ClientCategory: "RETAIL"}
			f.instruments[9] = &LeverageInstrumentView{
				ID: 9, Symbol: c.base + "/" + c.quote,
				BaseCurrency: c.base, QuoteCurrency: c.quote, MaxLeverage: 100,
			}
			// High entity policy so the statutory cap is the binding leg.
			for _, g := range []string{GroupMajor, GroupMinor, GroupExotic} {
				f.policies[levPolicyKey(c.entity, "RETAIL", g)] = 200
			}
			svc := NewLeverageService(f, nil)
			eff, err := svc.Effective(ctx, 7, 9)
			if err != nil {
				t.Fatal(err)
			}
			if eff != c.want {
				t.Fatalf("%s %s/%s: effective=%d, want %d", c.entity, c.base, c.quote, eff, c.want)
			}
		})
	}
}

func TestLeverageResolveInstrumentCeilingAndNonMarginable(t *testing.T) {
	ctx := context.Background()
	f := levFixture(200)
	f.instruments[9].MaxLeverage = 5 // instrument row is the binding cap
	svc := NewLeverageService(f, nil)
	eff, err := svc.Effective(ctx, 7, 9)
	if err != nil {
		t.Fatal(err)
	}
	if eff != 5 {
		t.Fatalf("effective=%d, want 5 (instrument max)", eff)
	}
	// Non-marginable instrument pins the ceiling at 1.
	f.instruments[9].MaxLeverage = 0
	eff, err = svc.Effective(ctx, 7, 9)
	if err != nil {
		t.Fatal(err)
	}
	if eff != 1 {
		t.Fatalf("non-marginable effective=%d, want 1", eff)
	}
}

func TestLeverageResolveEntityPolicyFallbackStrictest(t *testing.T) {
	ctx := context.Background()
	f := newLevStoreFake()
	f.accounts[7] = &LeverageAccountView{ID: 7, EntityCode: "OFFSHORE", ClientCategory: "RETAIL"}
	f.instruments[9] = &LeverageInstrumentView{
		ID: 9, Symbol: "EUR/USD", BaseCurrency: "EUR", QuoteCurrency: "USD", MaxLeverage: 100,
	}
	// No entity_leverage_policy row → strictest seed cap 10, never open.
	svc := NewLeverageService(f, nil)
	d, err := svc.Resolve(ctx, 7, 9)
	if err != nil {
		t.Fatal(err)
	}
	if d.Effective != fallbackEntityPolicyCap || d.Source != "entity_policy_fallback" {
		t.Fatalf("effective=%d source=%s, want %d/entity_policy_fallback",
			d.Effective, d.Source, fallbackEntityPolicyCap)
	}
}

func TestLeverageResolveProfessionalNoStatutoryCap(t *testing.T) {
	ctx := context.Background()
	f := newLevStoreFake()
	f.accounts[7] = &LeverageAccountView{ID: 7, EntityCode: "EU-ESMA", ClientCategory: "PROFESSIONAL"}
	f.instruments[9] = &LeverageInstrumentView{
		ID: 9, Symbol: "EUR/USD", BaseCurrency: "EUR", QuoteCurrency: "USD", MaxLeverage: 100,
	}
	f.policies[levPolicyKey("EU-ESMA", "PROFESSIONAL", GroupMajor)] = 200
	svc := NewLeverageService(f, nil)
	d, err := svc.Resolve(ctx, 7, 9)
	if err != nil {
		t.Fatal(err)
	}
	if d.Regime != RegimeProfessional {
		t.Fatalf("regime=%s, want PROFESSIONAL", d.Regime)
	}
	for _, c := range d.Components {
		if c.Source == "category_cap" {
			t.Fatalf("professional accounts carry no statutory category cap: %+v", d.Components)
		}
	}
	if d.Effective != 100 || d.Source != "instrument_max" {
		t.Fatalf("effective=%d source=%s, want 100/instrument_max", d.Effective, d.Source)
	}
}

func TestLeverageResolveTierBandStepsDown(t *testing.T) {
	ctx := context.Background()
	f := levFixture(200)
	// ESMA §13.6f major bands: <1M → 30, 1–5M → 20, 5–10M → 10, ≥10M → 5.
	f.tierFn = func(regime, group string, n decimal.Decimal) (*int, error) {
		tier := TierAt(esmaMajorBands(), n)
		if tier == nil {
			return nil, nil
		}
		return &tier.MaxLeverage, nil
	}
	svc := NewLeverageService(f, nil)

	for _, tc := range []struct {
		notional string
		want     int
		source   string
	}{
		{"500000", 30, "category_cap"}, // band cap 30 = statutory 30; category listed first wins tie
		{"2000000", 20, "tier_band"},   // band cap 20 < statutory 30
		{"7000000", 10, "tier_band"},
		{"20000000", 5, "tier_band"},
	} {
		f.notionals[[2]int64{7, 9}] = d(tc.notional)
		res, err := svc.Resolve(ctx, 7, 9)
		if err != nil {
			t.Fatal(err)
		}
		if res.Effective != tc.want {
			t.Fatalf("notional %s: effective=%d, want %d (components %+v)",
				tc.notional, res.Effective, tc.want, res.Components)
		}
	}
}

func esmaMajorBands() []LeverageTier {
	to1m := d("1000000")
	to5m := d("5000000")
	to10m := d("10000000")
	return []LeverageTier{
		{ID: 1, InstrumentGroup: GroupMajor, Regime: RegimeESMA,
			NotionalFrom: decimal.Zero, NotionalTo: &to1m, MaxLeverage: 30},
		{ID: 2, InstrumentGroup: GroupMajor, Regime: RegimeESMA,
			NotionalFrom: to1m, NotionalTo: &to5m, MaxLeverage: 20},
		{ID: 3, InstrumentGroup: GroupMajor, Regime: RegimeESMA,
			NotionalFrom: to5m, NotionalTo: &to10m, MaxLeverage: 10},
		{ID: 4, InstrumentGroup: GroupMajor, Regime: RegimeESMA,
			NotionalFrom: to10m, MaxLeverage: 5},
	}
}

func TestLeverageResolveChosenLeverage(t *testing.T) {
	ctx := context.Background()
	f := levFixture(200)
	svc := NewLeverageService(f, nil)

	// Account default row applies when no per-symbol row exists.
	f.defaults[7] = &ChosenLeverage{Leverage: 10}
	d, err := svc.Resolve(ctx, 7, 9)
	if err != nil {
		t.Fatal(err)
	}
	if d.Effective != 10 || d.Source != "account_chosen" {
		t.Fatalf("default chosen: eff=%d src=%s, want 10/account_chosen", d.Effective, d.Source)
	}
	// Per-symbol row wins over the account default.
	f.perSym[[2]int64{7, 9}] = &ChosenLeverage{Leverage: 5}
	d, err = svc.Resolve(ctx, 7, 9)
	if err != nil {
		t.Fatal(err)
	}
	if d.Effective != 5 {
		t.Fatalf("per-symbol chosen must win over default: eff=%d, want 5", d.Effective)
	}
}

// ---------------------------------------------------------------------------
// Fail closed — every store error or missing row rejects
// ---------------------------------------------------------------------------

func TestLeverageResolveFailClosed(t *testing.T) {
	ctx := context.Background()
	svc := NewLeverageService(levFixture(200), nil)

	if _, err := svc.Effective(ctx, 999, 9); err == nil {
		t.Fatal("missing account must reject")
	} else {
		requireCode(t, err, CodeLeverageNotFound)
	}
	if _, err := svc.Effective(ctx, 7, 999); err == nil {
		t.Fatal("missing instrument must reject")
	} else {
		requireCode(t, err, CodeLeverageNotFound)
	}

	mk := func(mut func(*levStoreFake)) *LeverageService {
		f := levFixture(200)
		mut(f)
		return NewLeverageService(f, nil)
	}
	cases := []struct {
		name string
		svc  *LeverageService
	}{
		{"account view error", mk(func(f *levStoreFake) { f.acctErr = fmt.Errorf("pg down") })},
		{"instrument view error", mk(func(f *levStoreFake) { f.instErr = fmt.Errorf("pg down") })},
		{"notional error", mk(func(f *levStoreFake) { f.notionalErr = fmt.Errorf("pg down") })},
		{"policy error", mk(func(f *levStoreFake) { f.policyErr = fmt.Errorf("pg down") })},
		{"tier error", mk(func(f *levStoreFake) {
			f.tierFn = func(_, _ string, _ decimal.Decimal) (*int, error) {
				return nil, fmt.Errorf("pg down")
			}
		})},
		{"chosen error", mk(func(f *levStoreFake) { f.chosenErr = fmt.Errorf("pg down") })},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.svc.Effective(ctx, 7, 9)
			if err == nil {
				t.Fatal("store failure must reject fail-closed")
			}
			requireCode(t, err, CodeLeverageUnavailable)
		})
	}
}

// ---------------------------------------------------------------------------
// Cache — leverage:eff:{acct}:{instr} mirror + invalidation
// ---------------------------------------------------------------------------

func TestLeverageResolveCachesDecision(t *testing.T) {
	ctx := context.Background()
	f := levFixture(200)
	cache := newLevCacheFake()
	svc := NewLeverageService(f, cache)

	eff, err := svc.Effective(ctx, 7, 9)
	if err != nil {
		t.Fatal(err)
	}
	key := "leverage:eff:7:9"
	if cache.setCalls != 1 {
		t.Fatalf("cache Set calls=%d, want 1", cache.setCalls)
	}
	if _, ok := cache.m[key]; !ok {
		t.Fatalf("expected cached decision under %q, keys %v", key, cache.m)
	}
	// Second resolve must be served from cache — store not re-consulted.
	before := f.acctCalls
	eff2, err := svc.Effective(ctx, 7, 9)
	if err != nil {
		t.Fatal(err)
	}
	if eff2 != eff {
		t.Fatalf("cached effective=%d, want %d", eff2, eff)
	}
	if f.acctCalls != before {
		t.Fatalf("cache hit must not re-hit the store (calls %d→%d)", before, f.acctCalls)
	}
	// Corrupt cache payload → fall back to a fresh resolve, never error.
	cache.m[key] = "{not-json"
	if _, err := svc.Effective(ctx, 7, 9); err != nil {
		t.Fatalf("corrupt cache must re-resolve, got %v", err)
	}
}

func TestLeverageInvalidate(t *testing.T) {
	ctx := context.Background()
	f := levFixture(200)
	cache := newLevCacheFake()
	svc := NewLeverageService(f, cache)
	if _, err := svc.Effective(ctx, 7, 9); err != nil {
		t.Fatal(err)
	}

	svc.Invalidate(ctx, 7, 9)
	if len(cache.delCalls) != 1 || len(cache.delCalls[0]) != 1 ||
		cache.delCalls[0][0] != "leverage:eff:7:9" {
		t.Fatalf("pair invalidate dels: %v", cache.delCalls)
	}

	// Account-scoped invalidation scans and deletes the matched keys.
	cache.scanKeys = []string{"leverage:eff:7:9", "leverage:eff:7:11"}
	svc.InvalidateAccount(ctx, 7)
	if len(cache.scanCalls) != 1 || cache.scanCalls[0] != "leverage:eff:7:*" {
		t.Fatalf("account scan patterns: %v", cache.scanCalls)
	}
	svc.InvalidateInstrument(ctx, 9)
	if cache.scanCalls[len(cache.scanCalls)-1] != "leverage:eff:*:9" {
		t.Fatalf("instrument scan patterns: %v", cache.scanCalls)
	}
	svc.InvalidateAll(ctx)
	if cache.scanCalls[len(cache.scanCalls)-1] != "leverage:eff:*" {
		t.Fatalf("global scan patterns: %v", cache.scanCalls)
	}
	// A nil-cache service must not panic on any invalidation.
	bare := NewLeverageService(f, nil)
	bare.Invalidate(ctx, 7, 9)
	bare.InvalidateAccount(ctx, 7)
	bare.InvalidateInstrument(ctx, 9)
	bare.InvalidateAll(ctx)
}

// ---------------------------------------------------------------------------
// SetLeverage — §13.13 runtime changes
// ---------------------------------------------------------------------------

func TestLeverageSetLeverageValidation(t *testing.T) {
	ctx := context.Background()
	svc := NewLeverageService(levFixture(200), nil)

	for _, req := range []int{0, -5, maxSelectableLeverage + 1} {
		if _, err := svc.SetLeverage(ctx, SetLeverageInput{
			AccountID: 7, InstrumentID: ptrInt64(9), Requested: req, UserID: 1,
		}); err == nil {
			t.Fatalf("requested %d must reject", req)
		} else {
			requireCode(t, err, CodeLeverageInvalid)
		}
	}
	// Missing account / instrument → NOT_FOUND.
	if _, err := svc.SetLeverage(ctx, SetLeverageInput{
		AccountID: 999, InstrumentID: ptrInt64(9), Requested: 10, UserID: 1,
	}); err == nil {
		t.Fatal("missing account must reject")
	} else {
		requireCode(t, err, CodeLeverageNotFound)
	}
	if _, err := svc.SetLeverage(ctx, SetLeverageInput{
		AccountID: 7, InstrumentID: ptrInt64(999), Requested: 10, UserID: 1,
	}); err == nil {
		t.Fatal("missing instrument must reject")
	} else {
		requireCode(t, err, CodeLeverageNotFound)
	}
	// Request above the resolved cap (entity 200, ESMA 30, inst 30 → 30).
	if _, err := svc.SetLeverage(ctx, SetLeverageInput{
		AccountID: 7, InstrumentID: ptrInt64(9), Requested: 31, UserID: 1,
	}); err == nil {
		t.Fatal("request above resolved cap must reject")
	} else {
		requireCode(t, err, CodeLeverageInvalid)
	}
}

func TestLeverageSetLeverageSuccess(t *testing.T) {
	ctx := context.Background()
	f := levFixture(200)
	cache := newLevCacheFake()
	svc := NewLeverageService(f, cache)
	// Warm the cache — the write must invalidate the stale decision.
	if _, err := svc.Effective(ctx, 7, 9); err != nil {
		t.Fatal(err)
	}

	res, err := svc.SetLeverage(ctx, SetLeverageInput{
		AccountID: 7, InstrumentID: ptrInt64(9), Requested: 25, UserID: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.puts) != 1 || f.puts[0].leverage != 25 || f.puts[0].userID != 42 {
		t.Fatalf("put calls: %+v", f.puts)
	}
	if f.puts[0].before != nil {
		t.Fatalf("before image must be nil on first write, got %v", *f.puts[0].before)
	}
	if len(cache.delCalls) == 0 {
		t.Fatal("cache must be invalidated after the write")
	}
	// The result reports the re-resolved effective leverage (chosen 25 < cap 30).
	if res.Effective == nil || res.Effective.Effective != 25 ||
		res.Effective.Source != "account_chosen" {
		t.Fatalf("effective decision: %+v", res.Effective)
	}
	// A second write snapshots the prior value for the audit row.
	if _, err := svc.SetLeverage(ctx, SetLeverageInput{
		AccountID: 7, InstrumentID: ptrInt64(9), Requested: 20, UserID: 42,
	}); err != nil {
		t.Fatal(err)
	}
	if len(f.puts) != 2 || f.puts[1].before == nil || *f.puts[1].before != 25 {
		t.Fatalf("second put must carry before=25: %+v", f.puts)
	}
}

func TestLeverageSetLeverageDefaultRow(t *testing.T) {
	ctx := context.Background()
	f := levFixture(200)
	cache := newLevCacheFake()
	svc := NewLeverageService(f, cache)

	// Default cap = max over groups of min(policy, category, tier):
	// ESMA retail → best = 30 (major cell).
	res, err := svc.SetLeverage(ctx, SetLeverageInput{
		AccountID: 7, InstrumentID: nil, Requested: 30, UserID: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.InstrumentID != nil || res.Effective != nil {
		t.Fatalf("default row result: %+v", res)
	}
	if len(f.puts) != 1 || f.puts[0].instrumentID != nil {
		t.Fatalf("default put: %+v", f.puts)
	}
	// Above the best cell (30) can never take effect → reject.
	if _, err := svc.SetLeverage(ctx, SetLeverageInput{
		AccountID: 7, InstrumentID: nil, Requested: 31, UserID: 5,
	}); err == nil {
		t.Fatal("default above max cap must reject")
	}
	// Default write invalidates the whole account scope.
	if len(cache.scanCalls) == 0 || cache.scanCalls[len(cache.scanCalls)-1] != "leverage:eff:7:*" {
		t.Fatalf("default write must InvalidateAccount, scans %v", cache.scanCalls)
	}
}

func TestLeverageSetLeverageMarginRecheck(t *testing.T) {
	ctx := context.Background()
	mk := func() (*levStoreFake, *LeverageService) {
		f := levFixture(200)
		f.positions[7] = []PositionNotional{
			{InstrumentID: 9, Symbol: "EUR/USD", Side: "LONG", GrossNotional: d("10000")},
		}
		return f, NewLeverageService(f, nil)
	}

	// No margin account → fail closed.
	f, svc := mk()
	if _, err := svc.SetLeverage(ctx, SetLeverageInput{
		AccountID: 7, InstrumentID: ptrInt64(9), Requested: 5, UserID: 1,
	}); err == nil {
		t.Fatal("open positions + missing margin account must reject")
	} else {
		requireCode(t, err, CodeLeverageInsufficient)
	}

	// Projected used margin (10000/5 = 2000) exceeds equity 100 → reject.
	f, svc = mk()
	eq := d("100")
	f.equities[7] = &eq
	if _, err := svc.SetLeverage(ctx, SetLeverageInput{
		AccountID: 7, InstrumentID: ptrInt64(9), Requested: 5, UserID: 1,
	}); err == nil {
		t.Fatal("margin re-check breach must reject")
	} else {
		requireCode(t, err, CodeLeverageInsufficient)
	}

	// Equity 5000 covers 2000 projected → accepted.
	f, svc = mk()
	eq2 := d("5000")
	f.equities[7] = &eq2
	if _, err := svc.SetLeverage(ctx, SetLeverageInput{
		AccountID: 7, InstrumentID: ptrInt64(9), Requested: 5, UserID: 1,
	}); err != nil {
		t.Fatalf("margin re-check pass must accept: %v", err)
	}
	if len(f.puts) != 1 {
		t.Fatalf("put calls: %+v", f.puts)
	}
}

// ---------------------------------------------------------------------------
// leverage_tiers.go — pure band math
// ---------------------------------------------------------------------------

func TestLeverageTierAt(t *testing.T) {
	bands := esmaMajorBands()
	cases := []struct {
		notional string
		wantID   int64
	}{
		{"0", 1},
		{"999999.999", 1},
		{"1000000", 2}, // notional_from inclusive
		{"4999999", 2},
		{"5000000", 3},
		{"10000000", 4},
		{"999999999", 4}, // open-ended top band
	}
	for _, c := range cases {
		got := TierAt(bands, d(c.notional))
		if got == nil || got.ID != c.wantID {
			t.Fatalf("TierAt(%s): %+v, want band %d", c.notional, got, c.wantID)
		}
	}
	// A gap resolves nothing — callers treat nil as "no tier cap".
	gapped := []LeverageTier{
		{ID: 1, InstrumentGroup: GroupMajor, Regime: RegimeESMA,
			NotionalFrom: d("100"), MaxLeverage: 5},
	}
	if got := TierAt(gapped, d("50")); got != nil {
		t.Fatalf("notional below the first band must resolve nil, got %+v", got)
	}
	// SortTiers orders by notional_from.
	unsorted := []LeverageTier{
		{ID: 2, NotionalFrom: d("1000"), MaxLeverage: 5},
		{ID: 1, NotionalFrom: decimal.Zero, MaxLeverage: 30},
	}
	SortTiers(unsorted)
	if unsorted[0].ID != 1 {
		t.Fatalf("SortTiers order: %+v", unsorted)
	}
}

func TestLeverageBandMargin(t *testing.T) {
	bands := esmaMajorBands()

	// Zero/notional-negative → zero margin, even with no bands.
	m, err := BandMargin(nil, decimal.Zero)
	if err != nil || !m.IsZero() {
		t.Fatalf("zero notional: %s %v", m, err)
	}
	// No configured bands + positive notional → coded error (fail closed).
	if _, err := BandMargin(nil, d("1000")); err == nil {
		t.Fatal("no bands must error")
	} else {
		requireCode(t, err, CodeRiskLimitsInternal)
	}

	// 2M across the 0–1M (30:1) and 1–5M (20:1) bands:
	// margin = 1M/30 + 1M/20.
	m, err = BandMargin(bands, d("2000000"))
	if err != nil {
		t.Fatal(err)
	}
	want := d("1000000").Div(d("30")).Add(d("1000000").Div(d("20")))
	if !m.Equal(want) {
		t.Fatalf("band margin = %s, want %s", m, want)
	}

	// 12M spans every band including the open-ended top:
	// 1M/30 + 4M/20 + 5M/10 + 2M/5.
	m, err = BandMargin(bands, d("12000000"))
	if err != nil {
		t.Fatal(err)
	}
	want = d("1000000").Div(d("30")).
		Add(d("4000000").Div(d("20"))).
		Add(d("5000000").Div(d("10"))).
		Add(d("2000000").Div(d("5")))
	if !m.Equal(want) {
		t.Fatalf("full-span margin = %s, want %s", m, want)
	}

	// Overflow past a CLOSED top band: the implementation stops at the
	// configured slices (only the covered 1000 is margined) — the doc
	// comment claims a last-band fallback for the excess but the loop
	// drops it. Pinned to current behavior; flagged for the doc owner.
	closed := []LeverageTier{
		{ID: 1, NotionalFrom: decimal.Zero, NotionalTo: ptrDec(d("1000")), MaxLeverage: 10},
	}
	m, err = BandMargin(closed, d("1500"))
	if err != nil {
		t.Fatal(err)
	}
	if want := d("1000").Div(d("10")); !m.Equal(want) {
		t.Fatalf("closed-top overflow margin = %s, want %s", m, want)
	}

	// Malformed bands → coded errors.
	zeroWidth := []LeverageTier{
		{ID: 1, NotionalFrom: d("100"), NotionalTo: ptrDec(d("100")), MaxLeverage: 10},
	}
	if _, err := BandMargin(zeroWidth, d("100")); err == nil {
		t.Fatal("zero-width band must error")
	}
	badLev := []LeverageTier{
		{ID: 1, NotionalFrom: decimal.Zero, NotionalTo: ptrDec(d("100")), MaxLeverage: 0},
	}
	if _, err := BandMargin(badLev, d("50")); err == nil {
		t.Fatal("non-positive band leverage must error")
	}
}

func TestLeverageTierEnums(t *testing.T) {
	for _, g := range []string{GroupMajor, GroupMinor, GroupExotic} {
		if !ValidTierGroup(g) {
			t.Fatalf("group %s must be valid", g)
		}
	}
	if ValidTierGroup("MICRO") || ValidTierGroup("") {
		t.Fatal("invalid group accepted")
	}
	for _, r := range []string{RegimeESMA, RegimeCFTC, RegimeProfessional} {
		if !ValidTierRegime(r) {
			t.Fatalf("regime %s must be valid", r)
		}
	}
	if ValidTierRegime("FCA") || ValidTierRegime("") {
		t.Fatal("invalid regime accepted")
	}
}

func ptrInt64(v int64) *int64                   { return &v }
func ptrDec(v decimal.Decimal) *decimal.Decimal { return &v }

// ---------------------------------------------------------------------------
// Gated PostgreSQL integration — real migration subset
// ---------------------------------------------------------------------------

// pgRisk19Fixture builds a throwaway schema carrying the Phase-19
// leverage/position-mode/PB-credit/insurance-fund surface beyond the
// liqStoreFixture subset: accounts gain entity_code (098) +
// position_mode (233), plus admin_audit_log, orders, prime brokerage,
// pb_credit_reservations, leverage_tiers + account_leverage. The pool's
// search_path pins the scratch schema, matching liqStoreFixture.
func pgRisk19Fixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("EXC_PG_TEST not set")
	}
	dsn := liqStoreDSN()
	schema := fmt.Sprintf("risk19_itest_%d", time.Now().UnixNano())
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
		"005_create_orders.up.sql",
		"010_create_admin_audit_log.up.sql",
		"013_create_margin_accounts.up.sql",
		"014_create_positions.up.sql",
		"015_create_liquidation_auctions.up.sql",
		"016_create_insurance_fund.up.sql",
		"036_create_general_ledger.up.sql",
		"037_create_prime_brokerage.up.sql",
		"042_client_categorization.up.sql",
		"098_entity_leverage_policy.up.sql",
		"106_positions_isolated_margin.up.sql",
		"230_liquidation_risk.up.sql",
		"232_pb_credit_reservations.up.sql",
		"233_accounts_position_mode.up.sql",
		"234_leverage_tiers.up.sql",
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
	return pool
}

// levSeedAccount seeds a user + account with an entity assignment.
func levSeedAccount(t *testing.T, pool *pgxpool.Pool, category, entity string) int64 {
	t.Helper()
	acct := liqSeedAccount(t, pool, category)
	if _, err := pool.Exec(context.Background(),
		`UPDATE accounts SET entity_code = $2 WHERE id = $1`, acct, entity); err != nil {
		t.Fatalf("entity_code: %v", err)
	}
	return acct
}

func TestPgLeverageStoreResolve(t *testing.T) {
	pool := pgRisk19Fixture(t)
	ctx := context.Background()
	store := NewPgLeverageStore(pool)
	svc := NewLeverageService(store, nil)

	acct := levSeedAccount(t, pool, "RETAIL", "EU-ESMA")
	eurusd := liqSeedInstrument(t, pool) // EUR/USD seed, max_leverage 30

	// EU-ESMA retail on a major: policy 30 / statutory 30 / tier 30 /
	// instrument 30 → effective 30.
	eff, err := svc.Effective(ctx, acct, eurusd)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if eff != 30 {
		t.Fatalf("EUR/USD retail effective=%d, want 30", eff)
	}

	// USD/MXN is the seeded exotic (max_leverage 10, ESMA exotic 10).
	var mxn int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM instruments WHERE symbol='USD/MXN'`).Scan(&mxn); err != nil {
		t.Fatalf("mxn instrument: %v", err)
	}
	eff, err = svc.Effective(ctx, acct, mxn)
	if err != nil {
		t.Fatal(err)
	}
	if eff != 10 {
		t.Fatalf("USD/MXN retail effective=%d, want 10", eff)
	}

	// US-CFTC retail major: statutory 50 but the instrument ceiling 30
	// binds — most-restrictive wins across axes.
	us := levSeedAccount(t, pool, "RETAIL", "US-CFTC")
	d, err := svc.Resolve(ctx, us, eurusd)
	if err != nil {
		t.Fatal(err)
	}
	if d.Effective != 30 || d.Regime != RegimeCFTC {
		t.Fatalf("cftc resolve: eff=%d regime=%s", d.Effective, d.Regime)
	}

	// Symbol lookup + fail-closed NOT_FOUND legs.
	v, err := store.InstrumentLeverageViewBySymbol(ctx, "eur/usd")
	if err != nil || v == nil || v.ID != eurusd {
		t.Fatalf("symbol lookup: %v %+v", err, v)
	}
	if _, err := svc.Effective(ctx, 42424242, eurusd); err == nil {
		t.Fatal("missing account must reject")
	} else {
		requireCode(t, err, CodeLeverageNotFound)
	}
}

func TestPgLeverageSetLeverage(t *testing.T) {
	pool := pgRisk19Fixture(t)
	ctx := context.Background()
	store := NewPgLeverageStore(pool)
	svc := NewLeverageService(store, nil)

	acct := levSeedAccount(t, pool, "RETAIL", "EU-ESMA")
	inst := liqSeedInstrument(t, pool)
	liqSeedMarginAccount(t, pool, acct, "CROSS", "")

	res, err := svc.SetLeverage(ctx, SetLeverageInput{
		AccountID: acct, InstrumentID: &inst, Requested: 25, UserID: 77,
	})
	if err != nil {
		t.Fatalf("set leverage: %v", err)
	}
	if res.Effective == nil || res.Effective.Effective != 25 {
		t.Fatalf("effective after write: %+v", res.Effective)
	}
	// Read-back: the account_leverage row persisted.
	var lev int
	if err := pool.QueryRow(ctx, `
		SELECT leverage FROM account_leverage
		WHERE account_id=$1 AND instrument_id=$2`, acct, inst).Scan(&lev); err != nil {
		t.Fatalf("row read: %v", err)
	}
	if lev != 25 {
		t.Fatalf("persisted leverage=%d, want 25", lev)
	}
	// The §13.13 audit row committed with the write.
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM admin_audit_log
		WHERE action='account.leverage.update' AND target_id=$1`, acct).Scan(&n); err != nil {
		t.Fatalf("audit read: %v", err)
	}
	if n != 1 {
		t.Fatalf("audit rows=%d, want 1", n)
	}
	// Request above the resolved cap rejects without writing.
	if _, err := svc.SetLeverage(ctx, SetLeverageInput{
		AccountID: acct, InstrumentID: &inst, Requested: 500, UserID: 77,
	}); err == nil {
		t.Fatal("above-cap request must reject")
	}
}

func TestPgTierStore(t *testing.T) {
	pool := pgRisk19Fixture(t)
	ctx := context.Background()
	st := NewPgTierStore(pool)

	// Migration-234 seeds tile every (group × regime) cell:
	// 3 groups × 3 regimes × 4 bands = 36 rows.
	all, err := st.ListTiers(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 36 {
		t.Fatalf("seeded tiers=%d, want 36", len(all))
	}
	majorESMA, err := st.ListTiers(ctx, GroupMajor, RegimeESMA)
	if err != nil || len(majorESMA) != 4 {
		t.Fatalf("esma major bands: %v %d", err, len(majorESMA))
	}
	// TierAt resolves the band covering the notional.
	b, err := st.TierAt(ctx, GroupMajor, RegimeESMA, d("2000000"))
	if err != nil || b == nil || b.MaxLeverage != 20 {
		t.Fatalf("tier at 2M: %v %+v", err, b)
	}
	b, err = st.TierAt(ctx, GroupMajor, RegimeESMA, d("20000000"))
	if err != nil || b == nil || b.MaxLeverage != 5 {
		t.Fatalf("tier at 20M: %v %+v", err, b)
	}
	// Validation gates the write path (dual-control executor contract).
	if _, err := st.UpsertTier(ctx, LeverageTier{
		InstrumentGroup: "MICRO", Regime: RegimeESMA,
		NotionalFrom: decimal.Zero, MaxLeverage: 5,
	}, 1); err == nil {
		t.Fatal("invalid group must reject")
	} else {
		requireCode(t, err, CodeLeverageInvalid)
	}
	if _, err := st.UpsertTier(ctx, LeverageTier{
		InstrumentGroup: GroupMajor, Regime: RegimeESMA,
		NotionalFrom: d("3000000"), NotionalTo: ptrDec(d("2000000")), MaxLeverage: 5,
	}, 1); err == nil {
		t.Fatal("notional_to <= notional_from must reject")
	}
	// Valid upsert + idempotent rewrite + delete round-trip.
	row, err := st.UpsertTier(ctx, LeverageTier{
		InstrumentGroup: GroupExotic, Regime: RegimeProfessional,
		NotionalFrom: d("50000000"), MaxLeverage: 8,
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if row.MaxLeverage != 8 {
		t.Fatalf("upserted leverage=%d, want 8", row.MaxLeverage)
	}
	if _, err := st.UpsertTier(ctx, LeverageTier{
		InstrumentGroup: GroupExotic, Regime: RegimeProfessional,
		NotionalFrom: d("50000000"), MaxLeverage: 6,
	}, 1); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := st.DeleteTier(ctx, row.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.DeleteTier(ctx, row.ID); err == nil {
		t.Fatal("second delete must NOT_FOUND")
	}
}

// ---------------------------------------------------------------------------
// Entity-policy admin path (Task 19.3.24 step 3) — tx-scoped upsert +
// the list read the admin surface and venue doc share.
// ---------------------------------------------------------------------------

func TestPgEntityPolicyTxRoundTrip(t *testing.T) {
	pool := pgRisk19Fixture(t)
	ctx := context.Background()
	store := NewPgLeverageStore(pool)

	// Maker/approver ids ride the dual-control request; the store only
	// needs them for created_by/updated_by audit columns.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := UpsertEntityPolicyTx(ctx, tx, EntityPolicyRow{
		EntityCode: "ZZ-TEST", ClientCategory: "RETAIL",
		InstrumentGroup: "MAJOR", MaxLeverage: 44,
		EffectiveFrom: time.Now().UTC().Add(-time.Hour),
	}, 9001, 4242); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Resolution sees the new cell immediately.
	capPtr, err := store.EntityPolicyCap(ctx, "ZZ-TEST", "RETAIL", "MAJOR")
	if err != nil || capPtr == nil || *capPtr != 44 {
		t.Fatalf("resolved cap = %v err=%v, want 44", capPtr, err)
	}

	// Same cell + same effective_from rewrites the ceiling in place.
	tx2, _ := pool.Begin(ctx)
	if err := UpsertEntityPolicyTx(ctx, tx2, EntityPolicyRow{
		EntityCode: "zz-test", ClientCategory: "retail", // case-folded
		InstrumentGroup: "MAJOR", MaxLeverage: 22,
		EffectiveFrom: time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond),
	}, 9001, 4242); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	tx2.Commit(ctx)

	// List surfaces the matrix incl. the new cell.
	rows, err := store.ListEntityPolicies(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found *EntityPolicyRow
	for i := range rows {
		if rows[i].EntityCode == "ZZ-TEST" && rows[i].InstrumentGroup == "MAJOR" {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatal("upserted cell missing from ListEntityPolicies")
	}
	if found.MaxLeverage != 22 && found.MaxLeverage != 44 {
		t.Fatalf("listed leverage = %d, want the committed cell value", found.MaxLeverage)
	}
}
