// carry_trade_settlement_test.go — Task 3.3.15 coverage: hedged-leg net
// yield (positive, negative, zero), triple-swap day counts, per-currency
// journals, cumulative tracking, replay idempotency and fail-loud
// per-allocation errors.
package settlement

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"exchange/internal/ledger"
)

// ---------------------------------------------------------------------------
// Fake CarryStore — mirrors the PG invariants: yield row + running total
// commit atomically; replay on (allocation, accrual_date, currency)
// returns the stored cumulative without re-adding.
// ---------------------------------------------------------------------------

type fakeCarryStore struct {
	allocs  []CarryAllocation
	records []CarryYieldRecord
	totals  map[string]decimal.Decimal // "allocID|CCY" → cumulative
	err     error
}

func (s *fakeCarryStore) ActiveCarryAllocations(context.Context) ([]CarryAllocation, error) {
	return s.allocs, s.err
}

func (s *fakeCarryStore) RecordCarryYield(_ context.Context, rec CarryYieldRecord) (decimal.Decimal, error) {
	if s.totals == nil {
		s.totals = map[string]decimal.Decimal{}
	}
	for _, r := range s.records {
		if r.AllocationID == rec.AllocationID && r.Currency == rec.Currency &&
			r.AccrualDate.Equal(rec.AccrualDate) {
			return r.CumulativeYield, nil // replay — no re-accumulation
		}
	}
	key := fmt.Sprintf("%d|%s", rec.AllocationID, rec.Currency)
	cum := s.totals[key].Add(rec.NetYield)
	s.totals[key] = cum
	rec.CumulativeYield = cum
	s.records = append(s.records, rec)
	return cum, nil
}

// newTestCarryEngine wires a carry engine over the swap-engine fakes.
func newTestCarryEngine(t *testing.T, now time.Time, allocs ...CarryAllocation) (*CarrySettlementEngine, *fakeSwapRateStore, *fakePoster, *fakeCarryStore) {
	t.Helper()
	swaps, store, poster, _ := newTestSwapEngine(t, now)
	cs := &fakeCarryStore{allocs: allocs}
	eng, err := NewCarrySettlementEngine(swaps, cs)
	if err != nil {
		t.Fatalf("NewCarrySettlementEngine: %v", err)
	}
	return eng, store, poster, cs
}

func carryLeg(id, alloc, pos, instr int64, symbol, side, qty string) CarryLeg {
	return CarryLeg{
		ID: id, AllocationID: alloc, PositionID: pos, InstrumentID: instr,
		Symbol: symbol, Base: symbol[:3], Quote: symbol[4:], Side: side,
		Quantity: decimal.RequireFromString(qty),
		LotSize:  decimal.NewFromInt(100000),
	}
}

func seedCarryRate(store *fakeSwapRateStore, instr int64, eff time.Time, long, short string) {
	store.rates = append(store.rates, SwapRate{
		InstrumentID: instr, EffectiveDate: eff,
		LongPoints:  decimal.RequireFromString(long),
		ShortPoints: decimal.RequireFromString(short),
		Source:      "TEST",
	})
}

// ---------------------------------------------------------------------------
// Net yield across hedged legs
// ---------------------------------------------------------------------------

// Long EUR/USD earns +2.10/day; the short hedge leg pays −2.50/day → the
// bot's net yield is −0.40, settled to its sub-account as a debit.
func TestCarrySettleDay_HedgedNetNegative(t *testing.T) {
	now := utc(t, "2025-07-15T21:00:00Z") // Tuesday
	alloc := CarryAllocation{
		ID: 7, AccountID: 9001, BotRef: "carry-bot-1", Status: CarryActive,
		Legs: []CarryLeg{
			carryLeg(1, 7, 201, 1, "EUR/USD", "LONG", "100000"),
			carryLeg(2, 7, 202, 1, "EUR/USD", "SHORT", "100000"),
		},
	}
	eng, store, poster, cs := newTestCarryEngine(t, now, alloc)
	seedCarryRate(store, 1, day(t, "2025-07-15"), "0.000021", "-0.000025")

	rep, err := eng.SettleDay(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Allocations != 1 || rep.Settled != 1 || rep.Failed != 0 {
		t.Fatalf("report %+v", rep)
	}
	r := rep.Results[0]
	if r.NetYields["USD"] != "-0.4" || r.Cumulative["USD"] != "-0.4" {
		t.Fatalf("net/cum %v %v", r.NetYields, r.Cumulative)
	}
	if len(r.JournalIDs) != 1 || len(r.Legs) != 2 {
		t.Fatalf("result %+v", r)
	}
	// Negative net → bot sub-account debited: debit customer liability,
	// credit swap revenue; one balanced journal.
	j := poster.journals[0]
	if j.IdempotencyKey != "carry:7:20250715:USD" ||
		j.EntryType != ledger.EntryEODRollover {
		t.Fatalf("journal key=%q type=%s", j.IdempotencyKey, j.EntryType)
	}
	if err := j.Validate(); err != nil {
		t.Fatalf("carry journal invalid: %v", err)
	}
	if j.Lines[0].AccountCode != ledger.CustomerLiability("USD") || j.Lines[0].Debit.IsZero() {
		t.Fatalf("negative-net debit leg %+v", j.Lines[0])
	}
	// Audit record: gross split + leg detail.
	rec := cs.records[0]
	if !rec.GrossCredit.Equal(decimal.RequireFromString("2.1")) ||
		!rec.GrossDebit.Equal(decimal.RequireFromString("2.5")) ||
		!rec.NetYield.Equal(decimal.RequireFromString("-0.4")) ||
		rec.JournalEntryID == 0 || len(rec.LegDetail) != 2 {
		t.Fatalf("yield record %+v", rec)
	}
}

func TestCarrySettleDay_PositiveNetAndCumulative(t *testing.T) {
	now := utc(t, "2025-07-15T21:00:00Z")
	// Classic carry: LONG high-yield leg, half-size short hedge.
	alloc := CarryAllocation{
		ID: 8, AccountID: 9002, BotRef: "carry-bot-2", Status: CarryActive,
		Legs: []CarryLeg{
			carryLeg(3, 8, 203, 1, "EUR/USD", "LONG", "100000"),
			carryLeg(4, 8, 204, 1, "EUR/USD", "SHORT", "40000"),
		},
	}
	eng, store, poster, cs := newTestCarryEngine(t, now, alloc)
	seedCarryRate(store, 1, day(t, "2025-07-15"), "0.000021", "-0.000025")

	rep, err := eng.SettleDay(context.Background(), now)
	if err != nil || rep.Failed != 0 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	// +2.10 credit −1.00 debit = +1.10 net → bot credited.
	r := rep.Results[0]
	if r.NetYields["USD"] != "1.1" {
		t.Fatalf("net %v", r.NetYields)
	}
	j := poster.journals[0]
	if j.Lines[0].AccountCode != ledger.SwapRolloverRevenue("USD") || j.Lines[0].Debit.IsZero() {
		t.Fatalf("positive-net direction %+v", j.Lines[0])
	}
	if j.Effects[0].AccountID != 9002 || !j.Effects[0].AvailableDelta.Equal(decimal.RequireFromString("1.1")) {
		t.Fatalf("bot effect %+v", j.Effects[0])
	}

	// Day 2 (Wednesday — triple): same accrual, 3× legs, cumulative adds.
	wed := utc(t, "2025-07-16T21:00:00Z")
	seedCarryRate(store, 1, day(t, "2025-07-16"), "0.000021", "-0.000025")
	rep2, err := eng.SettleDay(context.Background(), wed)
	if err != nil || rep2.Failed != 0 {
		t.Fatalf("day2 rep=%+v err=%v", rep2, err)
	}
	if rep2.Results[0].NetYields["USD"] != "3.3" { // (2.1 − 1.0) × 3
		t.Fatalf("triple-day net %v", rep2.Results[0].NetYields)
	}
	if rep2.Results[0].Cumulative["USD"] != "4.4" { // 1.1 + 3.3
		t.Fatalf("cumulative %v", rep2.Results[0].Cumulative)
	}
	_ = cs
}

func TestCarrySettleDay_ZeroNetNoJournal(t *testing.T) {
	now := utc(t, "2025-07-15T21:00:00Z")
	// Two same-side legs netting to zero: +0.5 / −0.5 engineered by size.
	alloc := CarryAllocation{
		ID: 9, AccountID: 9003, BotRef: "carry-bot-3", Status: CarryActive,
		Legs: []CarryLeg{
			carryLeg(5, 9, 205, 1, "EUR/USD", "LONG", "50000"),  // +1.05
			carryLeg(6, 9, 206, 1, "EUR/USD", "SHORT", "42000"), // -1.05
		},
	}
	eng, store, poster, cs := newTestCarryEngine(t, now, alloc)
	seedCarryRate(store, 1, day(t, "2025-07-15"), "0.000021", "-0.000025")

	rep, err := eng.SettleDay(context.Background(), now)
	if err != nil || rep.Failed != 0 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	r := rep.Results[0]
	if r.NetYields["USD"] != "0" || len(r.JournalIDs) != 0 {
		t.Fatalf("zero-net result %+v", r)
	}
	if len(poster.journals) != 0 {
		t.Fatal("zero net must post no journal")
	}
	if len(cs.records) != 1 || cs.records[0].JournalEntryID != 0 {
		t.Fatalf("zero-net audit record %+v", cs.records)
	}
}

func TestCarrySettleDay_ReplayIdempotent(t *testing.T) {
	now := utc(t, "2025-07-15T21:00:00Z")
	alloc := CarryAllocation{
		ID: 10, AccountID: 9004, BotRef: "carry-bot-4", Status: CarryActive,
		Legs: []CarryLeg{carryLeg(7, 10, 207, 1, "EUR/USD", "LONG", "100000")},
	}
	eng, store, poster, cs := newTestCarryEngine(t, now, alloc)
	seedCarryRate(store, 1, day(t, "2025-07-15"), "0.000021", "-0.000025")

	if _, err := eng.SettleDay(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	rep, err := eng.SettleDay(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	// The journal idempotency key is stable — the GL replays it; the
	// yield record dedups on (allocation, date, currency) so cumulative
	// never double-counts.
	if rep.Results[0].Cumulative["USD"] != "2.1" || len(cs.records) != 1 {
		t.Fatalf("replay cumulative=%v records=%d",
			rep.Results[0].Cumulative, len(cs.records))
	}
	if poster.journals[0].IdempotencyKey != poster.journals[1].IdempotencyKey {
		t.Fatal("replay must reuse the same idempotency key")
	}
}

func TestCarrySettleDay_FailedAllocationPreserved(t *testing.T) {
	now := utc(t, "2025-07-15T21:00:00Z")
	good := CarryAllocation{
		ID: 11, AccountID: 9005, BotRef: "good", Status: CarryActive,
		Legs: []CarryLeg{carryLeg(8, 11, 208, 1, "EUR/USD", "LONG", "100000")},
	}
	bad := CarryAllocation{
		ID: 12, AccountID: 9006, BotRef: "bad", Status: CarryActive,
		// Instrument 99 has no swap rate → SWAP_RATE_STALE per leg.
		Legs: []CarryLeg{carryLeg(9, 12, 209, 99, "USD/JPY", "LONG", "100000")},
	}
	eng, store, _, cs := newTestCarryEngine(t, now, good, bad)
	seedCarryRate(store, 1, day(t, "2025-07-15"), "0.000021", "-0.000025")

	rep, err := eng.SettleDay(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Settled != 1 || rep.Failed != 1 {
		t.Fatalf("report %+v", rep)
	}
	var badRes, goodRes CarryAllocationResult
	for _, r := range rep.Results {
		if r.AllocationID == 12 {
			badRes = r
		} else {
			goodRes = r
		}
	}
	if badRes.Err == "" || !strings.Contains(badRes.Err, CodeSwapRateStale) {
		t.Fatalf("bad alloc result %+v", badRes)
	}
	if goodRes.NetYields["USD"] != "2.1" {
		t.Fatalf("good alloc net %v", goodRes.NetYields)
	}
	// Only the good allocation's yield record landed.
	if len(cs.records) != 1 || cs.records[0].AllocationID != 11 {
		t.Fatalf("records %+v", cs.records)
	}
	// Legless allocation fails loudly, never silently settles.
	eng2, _, _, _ := newTestCarryEngine(t, now, CarryAllocation{
		ID: 13, AccountID: 9007, BotRef: "legless", Status: CarryActive})
	rep2, err := eng2.SettleDay(context.Background(), now)
	if err != nil || rep2.Failed != 1 || rep2.Results[0].Err == "" {
		t.Fatalf("legless rep=%+v err=%v", rep2, err)
	}
}
