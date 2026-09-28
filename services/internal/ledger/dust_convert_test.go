// Unit tests for Task 3.3.20 — dust-balance conversion to base currency.
// Store, pricer and poster are faked; journals are validated structurally
// and against the seeded chart (no DB required).
package ledger

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeMidPricer struct {
	rates map[string]decimal.Decimal // "BASE→QUOTE"
	err   error
}

func (f *fakeMidPricer) MidRate(_ context.Context, base, quote string) (decimal.Decimal, error) {
	if f.err != nil {
		return decimal.Zero, f.err
	}
	r, ok := f.rates[base+"→"+quote]
	if !ok {
		return decimal.Zero, fmt.Errorf("no rate %s→%s", base, quote)
	}
	return r, nil
}

type fakeDustPoster struct {
	got []Journal
	res PostResult
	err error
}

func (f *fakeDustPoster) Post(_ context.Context, j Journal) (PostResult, error) {
	f.got = append(f.got, j)
	if f.err != nil {
		return PostResult{}, f.err
	}
	res := f.res
	if res.JournalID == 0 {
		res.JournalID = 9001
	}
	res.Committed = true
	return res, nil
}

type fakeDustStore struct {
	standing   bool
	base       string
	avail      map[string]decimal.Decimal // ccy → available
	locked     map[string]decimal.Decimal
	inst       DustInstrument
	instFound  bool
	spreadBps  decimal.Decimal
	spreadSet  bool
	sweeps     map[string]DustSweepRecord // "acct|ccy|YYYY-MM-DD"
	nextID     int64
	released   []int64
	completed  map[int64]int64 // sweepID → journalEntryID
	claimError error
}

func newFakeDustStore() *fakeDustStore {
	return &fakeDustStore{
		standing:  true,
		base:      "USD",
		avail:     map[string]decimal.Decimal{},
		locked:    map[string]decimal.Decimal{},
		sweeps:    map[string]DustSweepRecord{},
		completed: map[int64]int64{},
	}
}

func sweepKey(acct int64, ccy string, day time.Time) string {
	return fmt.Sprintf("%d|%s|%s", acct, ccy, day.Format("2006-01-02"))
}

func (f *fakeDustStore) AccountGoodStanding(context.Context, int64) (bool, error) {
	return f.standing, nil
}

func (f *fakeDustStore) AccountBaseCurrency(context.Context, int64) (string, error) {
	return f.base, nil
}

func (f *fakeDustStore) DustBalance(_ context.Context, _ int64, ccy string) (decimal.Decimal, decimal.Decimal, bool, error) {
	a, ok := f.avail[ccy]
	if !ok {
		return decimal.Zero, decimal.Zero, false, nil
	}
	return a, f.locked[ccy], true, nil
}

func (f *fakeDustStore) DustInstrument(context.Context, string, string) (DustInstrument, bool, error) {
	return f.inst, f.instFound, nil
}

func (f *fakeDustStore) ConversionSpreadBps(context.Context, int64) (decimal.Decimal, bool, error) {
	return f.spreadBps, f.spreadSet, nil
}

func (f *fakeDustStore) ClaimSweep(_ context.Context, rec DustSweepRecord) (bool, int64, error) {
	if f.claimError != nil {
		return false, 0, f.claimError
	}
	k := sweepKey(rec.AccountID, rec.Currency, rec.SweepDate)
	if _, exists := f.sweeps[k]; exists {
		return false, 0, nil
	}
	f.nextID++
	rec.ID = f.nextID
	f.sweeps[k] = rec
	return true, rec.ID, nil
}

func (f *fakeDustStore) SweepFor(_ context.Context, accountID int64, ccy string, day time.Time) (DustSweepRecord, bool, error) {
	r, ok := f.sweeps[sweepKey(accountID, ccy, day)]
	return r, ok, nil
}

func (f *fakeDustStore) CompleteSweep(_ context.Context, sweepID, journalEntryID int64) error {
	f.completed[sweepID] = journalEntryID
	for k, r := range f.sweeps {
		if r.ID == sweepID {
			r.JournalEntryID = journalEntryID
			f.sweeps[k] = r
		}
	}
	return nil
}

func (f *fakeDustStore) ReleaseSweep(_ context.Context, sweepID int64) error {
	f.released = append(f.released, sweepID)
	for k, r := range f.sweeps {
		if r.ID == sweepID {
			delete(f.sweeps, k)
		}
	}
	return nil
}

func newDust(t *testing.T, store *fakeDustStore, pricer *fakeMidPricer, poster *fakeDustPoster) *DustConverter {
	t.Helper()
	c, err := NewDustConverter(store, pricer, poster, func() time.Time {
		return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatalf("converter: %v", err)
	}
	return c
}

func eligibleFixture() (*fakeDustStore, *fakeMidPricer, *fakeDustPoster) {
	store := newFakeDustStore()
	store.avail["EUR"] = d("10")
	store.inst = DustInstrument{
		InstrumentID: 1, Symbol: "EUR/USD",
		BaseCurrency: "EUR", QuoteCurrency: "USD",
		MinNotional: d("1000"), // dust value 12 USD < 1000 USD → eligible
	}
	store.instFound = true
	store.spreadBps, store.spreadSet = d("50"), true
	pricer := &fakeMidPricer{rates: map[string]decimal.Decimal{"EUR→USD": d("1.2")}}
	return store, pricer, &fakeDustPoster{}
}

// ---------------------------------------------------------------------------
// Task 3.3.20 — eligibility + pricing + GL
// ---------------------------------------------------------------------------

// Eligible dust converts at mid − spread with balanced GL lines.
func TestDustSweepEligible(t *testing.T) {
	store, pricer, poster := eligibleFixture()
	c := newDust(t, store, pricer, poster)

	res, err := c.Sweep(context.Background(), 7, "EUR")
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// 10 EUR × 1.2 = 12.00 USD gross; 50bps spread → 11.94 credited, 0.06 revenue.
	if !res.DustAmount.Equal(d("10")) || !res.CreditedAmount.Equal(d("11.94")) {
		t.Fatalf("amounts wrong: %+v", res)
	}
	if !res.MidRate.Equal(d("1.2")) || !res.SpreadBps.Equal(d("50")) {
		t.Fatalf("rate/spread wrong: %+v", res)
	}
	if res.JournalID != 9001 || res.Replayed || res.Pending {
		t.Fatalf("result fields wrong: %+v", res)
	}

	j := poster.got[0]
	if err := j.Validate(); err != nil {
		t.Fatalf("journal invalid: %v", err)
	}
	if err := j.ValidateAccounts(DefaultChart()); err != nil {
		t.Fatalf("accounts unresolved: %v", err)
	}
	// Dust leg: DR 2010_CUSTOMER_LIABILITY_EUR / CR 1200_..._EUR.
	if j.Lines[0].AccountCode != CustomerLiability("EUR") || !j.Lines[0].Debit.Equal(d("10")) {
		t.Fatalf("dust debit leg: %+v", j.Lines[0])
	}
	if j.Lines[1].AccountCode != MultiCcyClearing("EUR") || !j.Lines[1].Credit.Equal(d("10")) {
		t.Fatalf("dust credit leg: %+v", j.Lines[1])
	}
	// Base leg: DR clearing 12.00 = CR client 11.94 + CR revenue 0.06.
	if j.Lines[2].AccountCode != MultiCcyClearing("USD") || !j.Lines[2].Debit.Equal(d("12")) {
		t.Fatalf("base debit leg: %+v", j.Lines[2])
	}
	if j.Lines[3].AccountCode != CustomerLiability("USD") || !j.Lines[3].Credit.Equal(d("11.94")) {
		t.Fatalf("base credit leg: %+v", j.Lines[3])
	}
	if j.Lines[4].AccountCode != ConversionSpreadRevenue("USD") || !j.Lines[4].Credit.Equal(d("0.06")) {
		t.Fatalf("spread revenue leg: %+v", j.Lines[4])
	}
	// Effects move the wallet: −10 EUR, +11.94 USD.
	if len(j.Effects) != 2 ||
		!j.Effects[0].AvailableDelta.Equal(d("-10")) ||
		!j.Effects[1].AvailableDelta.Equal(d("11.94")) {
		t.Fatalf("effects wrong: %+v", j.Effects)
	}
	// Completion back-filled journal_entry_id on the claim.
	if store.completed[res.SweepID] != 9001 {
		t.Fatalf("sweep not completed: %+v", store.completed)
	}
}

// Idempotency + daily rate limit: second sweep same (account, ccy, day)
// returns the recorded result without reposting.
func TestDustSweepIdempotentPerDay(t *testing.T) {
	store, pricer, poster := eligibleFixture()
	c := newDust(t, store, pricer, poster)

	if _, err := c.Sweep(context.Background(), 7, "EUR"); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	res, err := c.Sweep(context.Background(), 7, "EUR")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !res.Replayed || res.JournalID != 9001 {
		t.Fatalf("replay result wrong: %+v", res)
	}
	if len(poster.got) != 1 {
		t.Fatalf("replayed sweep must not repost, got %d journals", len(poster.got))
	}
}

// A crashed sweep (claimed, unposted) reports Pending — not Replayed.
func TestDustSweepPendingClaim(t *testing.T) {
	store, pricer, poster := eligibleFixture()
	poster.err = fmt.Errorf("db lost")
	c := newDust(t, store, pricer, poster)
	if _, err := c.Sweep(context.Background(), 7, "EUR"); err == nil {
		t.Fatalf("post failure must surface")
	}
	// Post failure released the claim — the day slot is free again.
	if len(store.released) != 1 {
		t.Fatalf("failed sweep must release its claim")
	}

	// An un-released crashed claim reports Pending (never silently replays
	// as a fresh sweep).
	day := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	store.sweeps[sweepKey(7, "EUR", day)] = DustSweepRecord{
		ID: 42, AccountID: 7, Currency: "EUR", BaseCurrency: "USD",
		SweepDate: day, DustAmount: d("10"), CreditedAmount: d("11.94"),
		MidRate: d("1.2"), SpreadBps: d("50"), JournalEntryID: 0,
	}
	res, err := c.Sweep(context.Background(), 7, "EUR")
	if err != nil {
		t.Fatalf("pending replay: %v", err)
	}
	if !res.Pending || res.Replayed || res.SweepID != 42 {
		t.Fatalf("pending claim must surface as Pending: %+v", res)
	}
}

// Ineligible paths: locked balance, above-threshold, bad standing,
// undisclosed spread, unpriceable pair — all coded rejections, no post.
func TestDustSweepIneligible(t *testing.T) {
	// locked by resting orders
	store, pricer, poster := eligibleFixture()
	store.locked["EUR"] = d("2")
	c := newDust(t, store, pricer, poster)
	if _, err := c.Sweep(context.Background(), 7, "EUR"); err == nil {
		t.Fatalf("locked balance must reject")
	} else {
		requireCode(t, err, CodeInvalidRequest)
	}

	// above min_notional → not dust
	store, pricer, poster = eligibleFixture()
	store.avail["EUR"] = d("1000") // 1200 USD >= 1000 min
	c = newDust(t, store, pricer, poster)
	if _, err := c.Sweep(context.Background(), 7, "EUR"); err == nil {
		t.Fatalf("above-threshold must reject")
	} else {
		requireCode(t, err, CodeInvalidRequest)
	}

	// bad standing
	store, pricer, poster = eligibleFixture()
	store.standing = false
	c = newDust(t, store, pricer, poster)
	if _, err := c.Sweep(context.Background(), 7, "EUR"); err == nil {
		t.Fatalf("bad standing must reject")
	} else {
		requireCode(t, err, CodeForbidden)
	}

	// no disclosed conversion spread → fail closed
	store, pricer, poster = eligibleFixture()
	store.spreadSet = false
	c = newDust(t, store, pricer, poster)
	if _, err := c.Sweep(context.Background(), 7, "EUR"); err == nil {
		t.Fatalf("undisclosed spread must reject")
	} else {
		requireCode(t, err, CodeConversionSpreadUndisclosed)
	}

	// oracle down → coded unavailable
	store, pricer, poster = eligibleFixture()
	pricer.err = fmt.Errorf("oracle timeout")
	c = newDust(t, store, pricer, poster)
	if _, err := c.Sweep(context.Background(), 7, "EUR"); err == nil {
		t.Fatalf("oracle failure must reject")
	} else {
		requireCode(t, err, CodeDustPriceUnavailable)
	}

	// base currency itself → invalid
	store, pricer, poster = eligibleFixture()
	c = newDust(t, store, pricer, poster)
	if _, err := c.Sweep(context.Background(), 7, "USD"); err == nil {
		t.Fatalf("base-ccy sweep must reject")
	} else {
		requireCode(t, err, CodeInvalidRequest)
	}
}

// min_notional in the dust ccy (inverted pair orientation) compares against
// the raw balance, not the converted value.
func TestDustSweepInvertedPairThreshold(t *testing.T) {
	store, pricer, poster := eligibleFixture()
	store.avail["USD"], store.base = d("50"), "EUR"
	delete(store.avail, "EUR")
	store.inst = DustInstrument{
		InstrumentID: 1, Symbol: "EUR/USD",
		BaseCurrency: "EUR", QuoteCurrency: "USD", // quote = dust ccy
		MinNotional: d("1000"), // 50 USD < 1000 USD → dust
	}
	pricer.rates = map[string]decimal.Decimal{"USD→EUR": d("0.8")}
	c := newDust(t, store, pricer, poster)
	res, err := c.Sweep(context.Background(), 7, "USD")
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// 50 USD × 0.8 = 40 EUR gross; 50bps → 39.8 credited.
	if !res.CreditedAmount.Equal(d("39.8")) {
		t.Fatalf("credited %s != 39.8", res.CreditedAmount)
	}
}
