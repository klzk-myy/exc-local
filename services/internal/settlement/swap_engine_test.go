// swap_engine_test.go — Task 3.3.11 coverage: DST-aware rollover clock,
// calendar-derived day counts (triple Wednesday, holiday 4x/5x), the
// charge formula, the full ProcessRollover pipeline (signs, negative
// rates, swap-free foregone, staleness gates, idempotency keys,
// post-commit notify), the feed ingester and the REST handler.
package settlement

import (
	"context"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"exchange/internal/ledger"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeSwapRateStore is an in-memory SwapRateStore.
type fakeSwapRateStore struct {
	rates    []SwapRate
	policies []ledger.SwapMarkupPolicy
	accruals []ledger.SwapAccrualRecord
}

func (s *fakeSwapRateStore) UpsertSwapRates(_ context.Context, rates []SwapRate) (int, error) {
	for _, r := range rates {
		replaced := false
		for i := range s.rates {
			if s.rates[i].InstrumentID == r.InstrumentID &&
				s.rates[i].EffectiveDate.Equal(r.EffectiveDate) {
				s.rates[i] = r
				replaced = true
			}
		}
		if !replaced {
			s.rates = append(s.rates, r)
		}
	}
	return len(rates), nil
}

func (s *fakeSwapRateStore) SwapRateFor(_ context.Context, instrumentID int64, date time.Time) (SwapRate, bool, error) {
	for _, r := range s.rates {
		if r.InstrumentID == instrumentID && r.EffectiveDate.Equal(normalizeDay(date)) {
			return r, true, nil
		}
	}
	return SwapRate{}, false, nil
}

func (s *fakeSwapRateStore) LatestSwapRate(_ context.Context, instrumentID int64, onOrBefore time.Time) (SwapRate, bool, error) {
	bound := normalizeDay(onOrBefore)
	var best SwapRate
	found := false
	for _, r := range s.rates {
		if r.InstrumentID == instrumentID && !r.EffectiveDate.After(bound) &&
			(!found || r.EffectiveDate.After(best.EffectiveDate)) {
			best, found = r, true
		}
	}
	return best, found, nil
}

func (s *fakeSwapRateStore) SwapRateHistory(_ context.Context, instrumentID int64, from, to time.Time, limit int) ([]SwapRate, error) {
	var out []SwapRate
	for _, r := range s.rates {
		if r.InstrumentID == instrumentID &&
			!r.EffectiveDate.Before(normalizeDay(from)) &&
			!r.EffectiveDate.After(normalizeDay(to)) {
			out = append(out, r)
		}
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].EffectiveDate.After(out[i].EffectiveDate) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *fakeSwapRateStore) ActiveSwapMarkupPolicies(context.Context) ([]ledger.SwapMarkupPolicy, error) {
	return s.policies, nil
}

func (s *fakeSwapRateStore) InsertSwapAccrual(_ context.Context, rec ledger.SwapAccrualRecord) (int64, error) {
	s.accruals = append(s.accruals, rec)
	return int64(len(s.accruals)), nil
}

// fakeIDResolver resolves canonical symbols from a static map.
type fakeIDResolver map[string]int64

func (m fakeIDResolver) InstrumentID(_ context.Context, symbol string) (int64, error) {
	if id, ok := m[NormalizeSymbol(symbol)]; ok {
		return id, nil
	}
	return 0, excerrors.New(codeNotFound, "unknown instrument "+symbol)
}

// newTestSwapEngine wires an engine over fakes; clock fixed at now.
func newTestSwapEngine(t *testing.T, now time.Time) (*SwapEngine, *fakeSwapRateStore, *fakePoster, *fakePub) {
	t.Helper()
	store := &fakeSwapRateStore{}
	poster := &fakePoster{}
	pub := &fakePub{}
	eng, err := NewSwapEngine(store, fakeIDResolver{"EUR/USD": 1, "USD/JPY": 2},
		testCalendar(t), poster, pub, nil, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewSwapEngine: %v", err)
	}
	return eng, store, poster, pub
}

func mkSwapPos(id, acct, instr int64, symbol, side, qty string, opened time.Time) SwapPosition {
	base, quote := symbol[:3], symbol[4:]
	return SwapPosition{
		PositionID: id, AccountID: acct, InstrumentID: instr,
		Symbol: symbol, Base: base, Quote: quote, Side: side,
		Quantity:   decimal.RequireFromString(qty),
		LotSize:    decimal.NewFromInt(100000),
		AccrualCcy: quote,
		OpenedAt:   opened,
	}
}

// ---------------------------------------------------------------------------
// RolloverClock — DST mapping derived, never hardcoded
// ---------------------------------------------------------------------------

func TestRolloverClock_CutoffOnDST(t *testing.T) {
	rc, err := DefaultRolloverClock()
	if err != nil {
		t.Fatal(err)
	}
	// Summer: 2025-07-01 17:00 EDT → 21:00 UTC.
	if got := rc.CutoffOn(utc(t, "2025-07-01T12:00:00Z")); got.UTC().Format("2006-01-02 15:04 MST") != "2025-07-01 21:00 UTC" {
		t.Fatalf("EDT cutoff = %s, want 21:00 UTC", got.UTC())
	}
	// Winter: 2025-01-15 17:00 EST → 22:00 UTC.
	if got := rc.CutoffOn(utc(t, "2025-01-15T12:00:00Z")); got.UTC().Format("2006-01-02 15:04 MST") != "2025-01-15 22:00 UTC" {
		t.Fatalf("EST cutoff = %s, want 22:00 UTC", got.UTC())
	}
	// DST transition sanity: the Sunday after March forward is still 17:00 local.
	fire := rc.NextCutoff(utc(t, "2025-03-09T10:00:00Z")) // spring-forward Sunday
	if et := fire.In(ny); et.Hour() != 17 {
		t.Fatalf("transition-day fire = %s ET", et)
	}
	// Exactly at the cutoff, LastCutoffOnOrBefore returns that instant.
	if got := rc.LastCutoffOnOrBefore(utc(t, "2025-07-01T21:00:00Z")); got.UTC().Format("2006-01-02 15:04") != "2025-07-01 21:00" {
		t.Fatalf("last cutoff at boundary = %s", got.UTC())
	}
	// RollDate normalizes the NY civil date to UTC midnight.
	if got := rc.RollDate(utc(t, "2025-07-01T21:30:00Z")); got.Format("2006-01-02") != "2025-07-01" {
		t.Fatalf("roll date = %s", got)
	}
}

func TestRolloverClock_RejectsBadConfig(t *testing.T) {
	if _, err := NewRolloverClock("Not/AZone", 17, 0); err == nil {
		t.Fatal("bad zone must fail")
	}
	if _, err := NewRolloverClock("America/New_York", 25, 0); err == nil {
		t.Fatal("bad hour must fail")
	}
}

// ---------------------------------------------------------------------------
// RolloverDays — calendar-derived day spans (never hardcoded weekday math)
// ---------------------------------------------------------------------------

func TestRolloverDays_PlainAndTripleWednesday(t *testing.T) {
	cal := testCalendar(t)
	cases := []struct {
		name, base, quote, roll string
		want                    int
	}{
		{"tuesday", "EUR", "USD", "2025-07-15", 1},   // Thu 07-17 → Fri 07-18
		{"wednesday", "EUR", "USD", "2025-07-16", 3}, // Fri 07-18 → Mon 07-21 (weekend)
		{"thursday", "EUR", "USD", "2025-07-17", 1},  // Mon 07-21 → Tue 07-22
		{"friday", "EUR", "USD", "2025-07-18", 1},    // Tue 07-22 → Wed 07-23
	}
	for _, tc := range cases {
		got, err := RolloverDays(cal, tc.base, tc.quote, day(t, tc.roll))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: RolloverDays(%s) = %d, want %d", tc.name, tc.roll, got, tc.want)
		}
	}
}

func TestRolloverDays_HolidayWeekend4x5x(t *testing.T) {
	cal := testCalendar(t)
	// USD/JPY roll Wednesday 2025-08-06: T+2 lands Fri Aug 8; Sat/Sun +
	// Monday Aug 11 Mountain Day (JPY) push the roll to Tue Aug 12 → 4×.
	got, err := RolloverDays(cal, "USD", "JPY", day(t, "2025-08-06"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 4 {
		t.Fatalf("holiday weekend roll = %d days, want 4", got)
	}
	// USD/JPY roll Wednesday 2025-04-30: T+2 lands Fri May 2; weekend +
	// Golden Week Mon May 5 + Tue May 6 push the roll to Wed May 7 → 5×.
	got, err = RolloverDays(cal, "USD", "JPY", day(t, "2025-04-30"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 5 {
		t.Fatalf("golden-week roll = %d days, want 5", got)
	}
	// EUR/USD roll Wednesday 2025-07-02: the July-4 USD holiday lengthens
	// the T+2 walk itself (Thu Jul 3 → Mon Jul 7), span Tue Jul 8 → 1×.
	got, err = RolloverDays(cal, "EUR", "USD", day(t, "2025-07-02"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("pre-weekend holiday roll = %d days, want 1", got)
	}
}

func TestRolloverDays_FailClosed(t *testing.T) {
	cal := testCalendar(t)
	if _, err := RolloverDays(cal, "EUR", "ZAR", day(t, "2025-07-15")); err == nil {
		t.Fatal("unknown currency must fail")
	}
	if _, err := RolloverDaysForLag(cal, "EUR", "USD", day(t, "2025-07-15"), 0); err == nil {
		t.Fatal("lag 0 must fail")
	}
	if _, err := RolloverDays(nil, "EUR", "USD", day(t, "2025-07-15")); err == nil {
		t.Fatal("nil calendar must fail")
	}
}

// ---------------------------------------------------------------------------
// InterbankSwapCharge — the task formula lots × points × lot_size × days
// ---------------------------------------------------------------------------

func TestInterbankSwapCharge(t *testing.T) {
	qty, lot := decimal.NewFromInt(100000), decimal.NewFromInt(100000)
	// 1 lot × 0.000021 × 100000 × 1 = 2.10 (quote-ccy units).
	got, err := InterbankSwapCharge(qty, lot, decimal.RequireFromString("0.000021"), 1)
	if err != nil || !got.Equal(decimal.RequireFromString("2.1")) {
		t.Fatalf("charge=%s err=%v, want 2.1", got, err)
	}
	// Triple day.
	got, _ = InterbankSwapCharge(qty, lot, decimal.RequireFromString("0.000021"), 3)
	if !got.Equal(decimal.RequireFromString("6.3")) {
		t.Fatalf("triple charge=%s, want 6.3", got)
	}
	// Negative points post symmetrically (negative policy rates).
	got, _ = InterbankSwapCharge(qty, lot, decimal.RequireFromString("-0.000025"), 1)
	if !got.Equal(decimal.RequireFromString("-2.5")) {
		t.Fatalf("negative charge=%s, want -2.5", got)
	}
	// Qty in units folds through lot_size identically: 200000 units @ lot
	// 100000 = 2 lots → 2 × pts × 100000.
	got, _ = InterbankSwapCharge(decimal.NewFromInt(200000), lot, decimal.RequireFromString("0.000021"), 1)
	if !got.Equal(decimal.RequireFromString("4.2")) {
		t.Fatalf("2-lot charge=%s, want 4.2", got)
	}
	for _, c := range []struct {
		name          string
		qty, lot, pts decimal.Decimal
		days          int
	}{
		{"zero days", qty, lot, decimal.NewFromInt(1), 0},
		{"zero qty", decimal.Zero, lot, decimal.NewFromInt(1), 1},
		{"zero lot", qty, decimal.Zero, decimal.NewFromInt(1), 1},
	} {
		if _, err := InterbankSwapCharge(c.qty, c.lot, c.pts, c.days); err == nil {
			t.Fatalf("%s must fail", c.name)
		}
	}
}

// ---------------------------------------------------------------------------
// ProcessRollover — the full per-position pipeline
// ---------------------------------------------------------------------------

func TestProcessRollover_LongShortSigns(t *testing.T) {
	now := utc(t, "2025-07-15T21:00:00Z") // Tuesday 17:00 EDT
	eng, store, poster, pub := newTestSwapEngine(t, now)
	store.rates = []SwapRate{{
		InstrumentID: 1, Symbol: "EUR/USD", EffectiveDate: day(t, "2025-07-15"),
		LongPoints:  decimal.RequireFromString("0.000021"),
		ShortPoints: decimal.RequireFromString("-0.000025"),
		Source:      "TEST",
	}}

	positions := []SwapPosition{
		mkSwapPos(101, 9001, 1, "EUR/USD", "LONG", "100000", utc(t, "2025-07-14T10:00:00Z")),
		mkSwapPos(102, 9002, 1, "EUR/USD", "SHORT", "100000", utc(t, "2025-07-14T10:00:00Z")),
	}
	rep, err := eng.ProcessRollover(context.Background(), positions, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Positions != 2 || rep.Accrued != 2 || rep.Failed != 0 {
		t.Fatalf("report %+v", rep)
	}
	if rep.RollDate.Format("2006-01-02") != "2025-07-15" {
		t.Fatalf("roll date %s", rep.RollDate)
	}

	long, short := rep.Results[0], rep.Results[1]
	if !long.Accrual.ClientDelta.Equal(decimal.RequireFromString("2.1")) {
		t.Fatalf("long delta=%s, want +2.1 (credit)", long.Accrual.ClientDelta)
	}
	if !short.Accrual.ClientDelta.Equal(decimal.RequireFromString("-2.5")) {
		t.Fatalf("short delta=%s, want -2.5 (debit)", short.Accrual.ClientDelta)
	}
	if long.Days != 1 || short.Days != 1 {
		t.Fatalf("days %d/%d, want 1", long.Days, short.Days)
	}

	// Balanced, idempotency-keyed journals through the poster.
	if len(poster.journals) != 2 {
		t.Fatalf("journals=%d, want 2", len(poster.journals))
	}
	if poster.journals[0].IdempotencyKey != "swap:101:20250715" ||
		poster.journals[1].IdempotencyKey != "swap:102:20250715" {
		t.Fatalf("idempotency keys: %q / %q",
			poster.journals[0].IdempotencyKey, poster.journals[1].IdempotencyKey)
	}
	if poster.journals[0].EntryType != ledger.EntryEODRollover {
		t.Fatalf("entry type %s", poster.journals[0].EntryType)
	}
	if err := poster.journals[0].Validate(); err != nil {
		t.Fatalf("journal invalid: %v", err)
	}
	// Audit rows landed with journal linkage.
	if len(store.accruals) != 2 || store.accruals[0].JournalEntryID == 0 {
		t.Fatalf("accrual records %+v", store.accruals)
	}
	// SWAP_CHARGED notifications published on the per-account subject.
	if len(pub.subjects) != 2 ||
		pub.subjects[0] != "account.swap.charged.9001" ||
		!strings.Contains(string(pub.payloads[0]), `"event_type":"SWAP_CHARGED"`) {
		t.Fatalf("notifications: %v", pub.subjects)
	}
}

func TestProcessRollover_NegativeLongRateDebits(t *testing.T) {
	now := utc(t, "2025-01-14T22:00:00Z") // Tuesday 17:00 EST
	eng, store, poster, _ := newTestSwapEngine(t, now)
	store.rates = []SwapRate{{
		InstrumentID: 1, EffectiveDate: day(t, "2025-01-14"),
		LongPoints:  decimal.RequireFromString("-0.000030"),
		ShortPoints: decimal.RequireFromString("0.000010"),
		Source:      "TEST",
	}}
	rep, err := eng.ProcessRollover(context.Background(), []SwapPosition{
		mkSwapPos(103, 9003, 1, "EUR/USD", "LONG", "100000", utc(t, "2025-01-13T10:00:00Z")),
	}, now)
	if err != nil || rep.Failed != 0 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	// Negative long points → the position is DEBITED -3.00.
	if !rep.Results[0].Accrual.ClientDelta.Equal(decimal.RequireFromString("-3")) {
		t.Fatalf("delta=%s, want -3", rep.Results[0].Accrual.ClientDelta)
	}
	// Negative-rate journal: client liability debited, revenue credited.
	j := poster.journals[0]
	if j.Lines[0].Debit.IsZero() || j.Lines[0].AccountCode != ledger.CustomerLiability("USD") {
		t.Fatalf("negative-rate journal lines %+v", j.Lines)
	}
}

func TestProcessRollover_TripleWednesday(t *testing.T) {
	now := utc(t, "2025-07-16T21:00:00Z") // Wednesday 17:00 EDT
	eng, store, _, _ := newTestSwapEngine(t, now)
	store.rates = []SwapRate{{
		InstrumentID: 1, EffectiveDate: day(t, "2025-07-16"),
		LongPoints: decimal.RequireFromString("0.000021"), Source: "TEST",
	}}
	rep, err := eng.ProcessRollover(context.Background(), []SwapPosition{
		mkSwapPos(104, 9004, 1, "EUR/USD", "LONG", "100000", utc(t, "2025-07-14T10:00:00Z")),
	}, now)
	if err != nil || rep.Failed != 0 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	r := rep.Results[0]
	if r.Days != 3 || !r.Accrual.ClientDelta.Equal(decimal.RequireFromString("6.3")) {
		t.Fatalf("triple roll days=%d delta=%s", r.Days, r.Accrual.ClientDelta)
	}
}

func TestProcessRollover_MarkupLegSeparate(t *testing.T) {
	now := utc(t, "2025-07-15T21:00:00Z")
	eng, store, poster, _ := newTestSwapEngine(t, now)
	store.rates = []SwapRate{{
		InstrumentID: 1, EffectiveDate: day(t, "2025-07-15"),
		LongPoints: decimal.RequireFromString("0.000021"), Source: "TEST",
	}}
	store.policies = []ledger.SwapMarkupPolicy{{
		InstrumentID: 0, LongMarkupBps: decimal.NewFromInt(10),
		Status: ledger.MarkupActive, ProposedBy: "a", ApprovedBy: "b",
	}}
	pos := mkSwapPos(105, 9005, 1, "EUR/USD", "LONG", "100000", utc(t, "2025-07-14T10:00:00Z"))
	pos.Notional = decimal.NewFromInt(360000) // markup = 360000×10/10⁴×1/360 = 1.0
	rep, err := eng.ProcessRollover(context.Background(), []SwapPosition{pos}, now)
	if err != nil || rep.Failed != 0 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	r := rep.Results[0]
	if !r.Accrual.MarkupAmount.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("markup=%s, want 1", r.Accrual.MarkupAmount)
	}
	if !r.Accrual.ClientDelta.Equal(decimal.RequireFromString("1.1")) {
		t.Fatalf("delta=%s, want 2.1-1.0=1.1", r.Accrual.ClientDelta)
	}
	// §5.21a: interbank and markup are SEPARATE balanced leg pairs.
	if len(poster.journals[0].Lines) != 4 {
		t.Fatalf("journal lines=%d, want 4 (2 legs)", len(poster.journals[0].Lines))
	}
}

func TestProcessRollover_SwapFreeForegone(t *testing.T) {
	now := utc(t, "2025-07-15T21:00:00Z")
	eng, store, poster, _ := newTestSwapEngine(t, now)
	store.rates = []SwapRate{{
		InstrumentID: 1, EffectiveDate: day(t, "2025-07-15"),
		LongPoints: decimal.RequireFromString("0.000021"), Source: "TEST",
	}}
	pos := mkSwapPos(106, 9006, 1, "EUR/USD", "LONG", "100000", utc(t, "2025-07-14T10:00:00Z"))
	pos.SwapFree = true
	rep, err := eng.ProcessRollover(context.Background(), []SwapPosition{pos}, now)
	if err != nil || rep.Failed != 0 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	r := rep.Results[0]
	// Zero applied, foregone reported, NO journal — but an audit row.
	if !r.Accrual.ClientDelta.IsZero() || !r.Accrual.SwapFree ||
		!r.Accrual.ForegoneAmount.Equal(decimal.RequireFromString("2.1")) {
		t.Fatalf("swapfree accrual %+v", r.Accrual)
	}
	if len(poster.journals) != 0 || r.JournalID != 0 {
		t.Fatalf("swap-free must post no journal: %d / id=%d", len(poster.journals), r.JournalID)
	}
	if len(store.accruals) != 1 {
		t.Fatal("swap-free accrual record must still be written")
	}
}

func TestProcessRollover_ZeroRateNoCharge(t *testing.T) {
	now := utc(t, "2025-07-15T21:00:00Z")
	eng, store, poster, _ := newTestSwapEngine(t, now)
	store.rates = []SwapRate{{
		InstrumentID: 1, EffectiveDate: day(t, "2025-07-15"),
		LongPoints: decimal.Zero, ShortPoints: decimal.Zero, Source: "TEST",
	}}
	rep, err := eng.ProcessRollover(context.Background(), []SwapPosition{
		mkSwapPos(107, 9007, 1, "EUR/USD", "LONG", "100000", utc(t, "2025-07-14T10:00:00Z")),
	}, now)
	if err != nil || rep.Failed != 0 || rep.Accrued != 1 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	if len(poster.journals) != 0 || !rep.Results[0].Accrual.ClientDelta.IsZero() {
		t.Fatalf("zero rate must post no journal")
	}
	if len(store.accruals) != 1 {
		t.Fatal("zero accrual must still hit the audit trail")
	}
}

func TestProcessRollover_OpenedAfterCutoff(t *testing.T) {
	now := utc(t, "2025-07-15T21:00:00Z")
	eng, store, poster, _ := newTestSwapEngine(t, now)
	store.rates = []SwapRate{{
		InstrumentID: 1, EffectiveDate: day(t, "2025-07-15"),
		LongPoints: decimal.RequireFromString("0.000021"), Source: "TEST",
	}}
	rep, err := eng.ProcessRollover(context.Background(), []SwapPosition{
		// Opened 22:00 UTC — AFTER the 21:00 UTC cutoff on this roll date.
		mkSwapPos(108, 9008, 1, "EUR/USD", "LONG", "100000", utc(t, "2025-07-15T22:00:00Z")),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Results[0].Skipped || rep.Results[0].SkipReason != "OPENED_AFTER_CUTOFF" {
		t.Fatalf("result %+v", rep.Results[0])
	}
	if len(poster.journals) != 0 || len(store.accruals) != 0 {
		t.Fatal("post-cutoff position must not accrue")
	}
}

func TestProcessRollover_StaleAndMissingRates(t *testing.T) {
	now := utc(t, "2025-07-15T21:00:00Z")
	eng, store, _, _ := newTestSwapEngine(t, now)
	// Rate 5 days old — outside the 3-day staleness bound.
	store.rates = []SwapRate{{
		InstrumentID: 1, EffectiveDate: day(t, "2025-07-10"),
		LongPoints: decimal.RequireFromString("0.000021"), Source: "TEST",
	}}
	rep, err := eng.ProcessRollover(context.Background(), []SwapPosition{
		mkSwapPos(109, 9009, 1, "EUR/USD", "LONG", "100000", utc(t, "2025-07-14T10:00:00Z")),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 1 || !strings.Contains(rep.Results[0].Err, CodeSwapRateStale) {
		t.Fatalf("stale result %+v", rep.Results[0])
	}

	// A rate inside the bound is used with RateStale flagged.
	store.rates = []SwapRate{{
		InstrumentID: 1, EffectiveDate: day(t, "2025-07-14"),
		LongPoints: decimal.RequireFromString("0.000021"), Source: "TEST",
	}}
	rep, err = eng.ProcessRollover(context.Background(), []SwapPosition{
		mkSwapPos(110, 9010, 1, "EUR/USD", "LONG", "100000", utc(t, "2025-07-14T10:00:00Z")),
	}, now)
	if err != nil || rep.Failed != 0 {
		t.Fatalf("fallback rep=%+v err=%v", rep, err)
	}
	if !rep.Results[0].RateStale {
		t.Fatal("fallback rate must flag RateStale")
	}

	// No rate at all → SWAP_RATE_STALE, never an implicit zero.
	store.rates = nil
	rep, _ = eng.ProcessRollover(context.Background(), []SwapPosition{
		mkSwapPos(111, 9011, 1, "EUR/USD", "LONG", "100000", utc(t, "2025-07-14T10:00:00Z")),
	}, now)
	if !strings.Contains(rep.Results[0].Err, CodeSwapRateStale) {
		t.Fatalf("missing rate result %+v", rep.Results[0])
	}
}

func TestProcessRollover_NotifyFailureNonFatal(t *testing.T) {
	now := utc(t, "2025-07-15T21:00:00Z")
	eng, store, poster, pub := newTestSwapEngine(t, now)
	pub.err = stderrors.New("nats down")
	store.rates = []SwapRate{{
		InstrumentID: 1, EffectiveDate: day(t, "2025-07-15"),
		LongPoints: decimal.RequireFromString("0.000021"), Source: "TEST",
	}}
	rep, err := eng.ProcessRollover(context.Background(), []SwapPosition{
		mkSwapPos(112, 9012, 1, "EUR/USD", "LONG", "100000", utc(t, "2025-07-14T10:00:00Z")),
	}, now)
	if err != nil || rep.Failed != 0 {
		t.Fatalf("notify failure must not fail the accrual: %+v %v", rep, err)
	}
	r := rep.Results[0]
	if r.NotifyErr == "" || r.JournalID == 0 || !r.Accrual.ClientDelta.Equal(decimal.RequireFromString("2.1")) {
		t.Fatalf("notify-fail result %+v", r)
	}
	_ = poster // journal committed — funds final regardless of notify
}

// ---------------------------------------------------------------------------
// SwapRateIngester — scheduled pull, row quarantine, fail-closed batch
// ---------------------------------------------------------------------------

type fakeSwapFeed struct {
	rows []swapRateWire
	err  error
}

func (f fakeSwapFeed) FetchSwapRates(context.Context, time.Time) ([]swapRateWire, error) {
	return f.rows, f.err
}

func TestSwapRateIngester_PullOnce(t *testing.T) {
	now := utc(t, "2025-07-15T21:00:00Z")
	store := &fakeSwapRateStore{}
	ing, err := NewSwapRateIngester(
		fakeSwapFeed{rows: []swapRateWire{
			{Symbol: "EURUSD", EffectiveDate: "2025-07-15",
				LongSwapPoints:  decimal.RequireFromString("0.000021"),
				ShortSwapPoints: decimal.RequireFromString("-0.000025"),
				Source:          "refinitiv"},
			{Symbol: "USD/JPY", EffectiveDate: "2025-07-15",
				LongSwapPoints:  decimal.RequireFromString("-0.000011"),
				ShortSwapPoints: decimal.RequireFromString("0.000008")},
			{Symbol: "", EffectiveDate: "2025-07-15"},        // malformed
			{Symbol: "EUR/USD", EffectiveDate: "not-a-date"}, // bad date
			{Symbol: "ZZZ/ZZZ", EffectiveDate: "2025-07-15"}, // unlisted
		}},
		store, fakeIDResolver{"EUR/USD": 1, "USD/JPY": 2},
		func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	rep, err := ing.PullOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fetched != 5 || rep.Stored != 2 || len(rep.Rejected) != 3 {
		t.Fatalf("report %+v", rep)
	}
	r, ok, _ := store.SwapRateFor(context.Background(), 1, day(t, "2025-07-15"))
	if !ok || r.Source != "REFINITIV" ||
		!r.LongPoints.Equal(decimal.RequireFromString("0.000021")) {
		t.Fatalf("stored rate %+v", r)
	}
	// Idempotent re-pull upserts rather than duplicates.
	if _, err := ing.PullOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range store.rates {
		if r.InstrumentID == 1 && r.EffectiveDate.Equal(day(t, "2025-07-15")) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("re-pull duplicated the rate row (%d)", n)
	}
}

func TestSwapRateIngester_FailsClosed(t *testing.T) {
	now := utc(t, "2025-07-15T21:00:00Z")
	store := &fakeSwapRateStore{}
	// All-invalid batch → SWAP_FEED_UNAVAILABLE, nothing stored.
	ing, _ := NewSwapRateIngester(
		fakeSwapFeed{rows: []swapRateWire{{Symbol: "BAD", EffectiveDate: "xx"}}},
		store, fakeIDResolver{}, func() time.Time { return now })
	rep, err := ing.PullOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), CodeSwapFeedUnavailable) {
		t.Fatalf("all-invalid pull: rep=%+v err=%v", rep, err)
	}
	// Transport failure propagates.
	ing2, _ := NewSwapRateIngester(
		fakeSwapFeed{err: stderrors.New("timeout")},
		store, fakeIDResolver{}, func() time.Time { return now })
	if _, err := ing2.PullOnce(context.Background()); err == nil {
		t.Fatal("feed failure must propagate")
	}
}

func TestSwapRateIngester_FileAndHTTPFeeds(t *testing.T) {
	sheet := `[{"symbol":"EUR/USD","effective_date":"2025-07-15","long_swap_points":"0.000021","short_swap_points":"-0.000025","source":"REFINITIV"}]`

	// File feed.
	dir := t.TempDir()
	path := filepath.Join(dir, "swaps.json")
	if err := os.WriteFile(path, []byte(sheet), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := (FileSwapRateFeed{Path: path}).FetchSwapRates(context.Background(), time.Now())
	if err != nil || len(rows) != 1 || rows[0].Symbol != "EUR/USD" {
		t.Fatalf("file feed: rows=%v err=%v", rows, err)
	}
	if _, err := (FileSwapRateFeed{Path: filepath.Join(dir, "missing.json")}).FetchSwapRates(context.Background(), time.Now()); err == nil {
		t.Fatal("missing file must fail")
	}

	// HTTP feed — effective_date query param plumbed through.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("effective_date") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sheet))
	}))
	defer srv.Close()
	rows, err = (HTTPSwapRateFeed{URL: srv.URL}).FetchSwapRates(
		context.Background(), day(t, "2025-07-15"))
	if err != nil || len(rows) != 1 {
		t.Fatalf("http feed: rows=%v err=%v", rows, err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer bad.Close()
	if _, err := (HTTPSwapRateFeed{URL: bad.URL}).FetchSwapRates(context.Background(), time.Now()); err == nil {
		t.Fatal("HTTP 502 must fail")
	}
}

// ---------------------------------------------------------------------------
// SwapRatesHandler — REST surface (Phase-05 wires the route)
// ---------------------------------------------------------------------------

func TestSwapRatesHandler(t *testing.T) {
	now := utc(t, "2025-07-15T21:00:00Z")
	eng, store, _, _ := newTestSwapEngine(t, now)
	store.rates = []SwapRate{
		{InstrumentID: 1, Symbol: "EUR/USD", EffectiveDate: day(t, "2025-07-14"),
			LongPoints:  decimal.RequireFromString("0.000020"),
			ShortPoints: decimal.RequireFromString("-0.000024"), Source: "TEST"},
		{InstrumentID: 1, Symbol: "EUR/USD", EffectiveDate: day(t, "2025-07-15"),
			LongPoints:  decimal.RequireFromString("0.000021"),
			ShortPoints: decimal.RequireFromString("-0.000025"), Source: "TEST"},
	}

	get := func(path, symbol string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.SetPathValue("symbol", symbol)
		rec := httptest.NewRecorder()
		eng.SwapRatesHandler(rec, req)
		return rec
	}

	// Exact date → 200, decimal-safe string fields.
	rec := get("/x?date=2025-07-15", "EUR/USD")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"long_swap_points":"0.000021"`) {
		t.Fatalf("exact: %d %s", rec.Code, rec.Body.String())
	}
	// Missing date → 404 RFC 7807.
	if rec := get("/x?date=2025-07-10", "EUR/USD"); rec.Code != 404 ||
		!strings.Contains(rec.Body.String(), "NOT_FOUND") {
		t.Fatalf("missing: %d %s", rec.Code, rec.Body.String())
	}
	// Bad date → 400.
	if rec := get("/x?date=bogus", "EUR/USD"); rec.Code != 400 {
		t.Fatalf("bad date: %d", rec.Code)
	}
	// Unknown symbol → resolver's NOT_FOUND maps to 404.
	if rec := get("/x", "USD/CHF"); rec.Code != 404 {
		t.Fatalf("unknown symbol: %d", rec.Code)
	}
	// Default latest → stale flag when effective < today.
	rec = get("/x", "EUR/USD")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"stale":true`) {
		t.Fatalf("latest same-day: %s", rec.Body.String())
	}
	// History range → two rows, newest first.
	rec = get("/x?from=2025-07-01&to=2025-07-31", "EURUSD")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "2025-07-15") ||
		!strings.Contains(rec.Body.String(), "2025-07-14") {
		t.Fatalf("history: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSwapRateWireValidation(t *testing.T) {
	// Points beyond DECIMAL(28,8) precision are rejected.
	if _, err := parseSwapRateWire(swapRateWire{
		Symbol:         "EUR/USD",
		EffectiveDate:  "2025-07-15",
		LongSwapPoints: decimal.RequireFromString("0.000000001"), // 9dp
	}, time.Now()); err == nil {
		t.Fatal("sub-quantum points must be rejected")
	}
	// Empty effective_date falls back to the pull date.
	r, err := parseSwapRateWire(swapRateWire{
		Symbol:         "EURUSD",
		LongSwapPoints: decimal.NewFromInt(1),
	}, day(t, "2025-07-15"))
	if err != nil || r.EffectiveDate.Format("2006-01-02") != "2025-07-15" ||
		r.Symbol != "EUR/USD" || r.Source != "MANUAL" {
		t.Fatalf("wire parse %+v err=%v", r, err)
	}
}
