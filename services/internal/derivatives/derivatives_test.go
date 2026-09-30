// derivatives_test.go — shared test fixtures: in-memory ContractStore,
// fake curve/spot feeds, and a holiday calendar with known closure dates.
package derivatives

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"exchange/internal/oracle"
	"exchange/internal/oracle/rates"
	"exchange/internal/settlement"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// day builds a UTC calendar date.
func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// testCalendar has exactly one holiday per currency so requireCurrencies
// passes, plus a known EUR closure for roll tests.
//   - USD: 2026-01-19 (MLK) and 2026-07-03 (Independence Day observed)
//   - EUR: 2026-01-19 is NOT an EUR holiday; EUR holiday 2026-05-01.
func testCalendar(t *testing.T) *settlement.HolidayCalendar {
	t.Helper()
	cal, err := settlement.NewHolidayCalendar([]settlement.Holiday{
		{Currency: "USD", Date: day(2026, 1, 19), Name: "MLK Day", Source: "FEDERAL_RESERVE"},
		{Currency: "EUR", Date: day(2026, 5, 1), Name: "Labour Day", Source: "TARGET2"},
		{Currency: "GBP", Date: day(2026, 5, 4), Name: "Early May BH", Source: "BANK_OF_ENGLAND"},
		{Currency: "JPY", Date: day(2026, 5, 6), Name: "Constitution Day", Source: "BANK_OF_JAPAN"},
		{Currency: "CAD", Date: day(2026, 7, 1), Name: "Canada Day", Source: "BANK_OF_CANADA"},
		{Currency: "MXN", Date: day(2026, 9, 16), Name: "Independence Day", Source: "BANXICO"},
		{Currency: "BRL", Date: day(2026, 9, 7), Name: "Independence Day", Source: "BCB"},
		{Currency: "CHF", Date: day(2026, 8, 1), Name: "National Day", Source: "SNB"},
		{Currency: "AUD", Date: day(2026, 1, 26), Name: "Australia Day", Source: "RBA"},
	})
	if err != nil {
		t.Fatalf("calendar: %v", err)
	}
	return cal
}

// completeCurve builds a spec-complete Curve with a flat rate.
func completeCurve(ccy string, rate float64, asOf time.Time) rates.Curve {
	r := decimal.NewFromFloat(rate)
	m := map[rates.Tenor]decimal.Decimal{}
	for _, t := range rates.CanonicalTenors {
		m[t] = r
	}
	return rates.Curve{Currency: ccy, Rates: m, AsOf: asOf}
}

// fakeCurves serves in-memory curves; absent keys error like the store.
type fakeCurves struct {
	m map[string]rates.Curve
}

func (f fakeCurves) GetCurve(_ context.Context, ccy string) (rates.Curve, error) {
	c, ok := f.m[ccy]
	if !ok {
		return rates.Curve{}, fmt.Errorf("%w: %s", rates.ErrCurveUnavailable, ccy)
	}
	return c, nil
}

type fakeSpot struct {
	m map[string]oracle.MarkView
}

func (f fakeSpot) Mark(_ context.Context, symbol string) (oracle.MarkView, error) {
	v, ok := f.m[symbol]
	if !ok {
		return oracle.MarkView{}, nil // absent — Found stays false
	}
	v.Found = true
	return v, nil
}

// ---------------------------------------------------------------------------
// In-memory ContractStore — mirrors the SERIALIZABLE semantics: single
// writer lock per InTx, idempotent contract insert by key.
// ---------------------------------------------------------------------------

type memStore struct {
	mu        sync.Mutex
	nextID    int64
	contracts map[int64]*Contract
	byKey     map[string]int64
	legs      map[int64][]PersistedLeg // contract_id → legs
	fixings   []NdfFixing
	seq       int64
}

func newMemStore() *memStore {
	return &memStore{contracts: map[int64]*Contract{}, byKey: map[string]int64{},
		legs: map[int64][]PersistedLeg{}, nextID: 1}
}

func (s *memStore) InTx(ctx context.Context, fn func(context.Context, ContractTx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(ctx, &memTx{s: s})
}

type memTx struct{ s *memStore }

func (t *memTx) InsertContract(_ context.Context, c *Contract) (int64, bool, error) {
	if c.IdempotencyKey != "" {
		if id, ok := t.s.byKey[c.IdempotencyKey]; ok {
			return id, false, nil
		}
	}
	cp := *c
	cp.ID = t.s.nextID
	t.s.nextID++
	t.s.contracts[cp.ID] = &cp
	if cp.IdempotencyKey != "" {
		t.s.byKey[cp.IdempotencyKey] = cp.ID
	}
	return cp.ID, true, nil
}

func (t *memTx) ContractForUpdate(_ context.Context, id int64) (*Contract, error) {
	c, ok := t.s.contracts[id]
	if !ok {
		return nil, excNotFound(id)
	}
	cp := *c
	return &cp, nil
}

func excNotFound(id int64) error {
	return excerrors.New(CodeNotFound, fmt.Sprintf("derivative contract %d not found", id))
}

func (t *memTx) InsertLegs(_ context.Context, legs []SettlementLeg) (int, error) {
	for _, l := range legs {
		t.s.seq++
		t.s.legs[l.ContractID] = append(t.s.legs[l.ContractID], PersistedLeg{
			ID: t.s.seq, Direction: l.Direction, Currency: l.Currency,
			Amount: l.Amount, ValueDate: l.ValueDate, Status: "PENDING",
		})
	}
	return len(legs), nil
}

func (t *memTx) ContractLegs(_ context.Context, contractID int64) ([]PersistedLeg, error) {
	return append([]PersistedLeg(nil), t.s.legs[contractID]...), nil
}

func (t *memTx) UpdateContractStatus(_ context.Context, id int64, st ContractStatus, settledAt *time.Time) error {
	c, ok := t.s.contracts[id]
	if !ok {
		return excNotFound(id)
	}
	c.Status = st
	c.SettledAt = settledAt
	return nil
}

func (t *memTx) InsertNdfFixing(_ context.Context, f *NdfFixing) (int64, error) {
	for _, g := range t.s.fixings {
		if g.ContractID == f.ContractID && g.FixingDate.Equal(f.FixingDate) {
			return g.ID, nil
		}
	}
	t.s.seq++
	f.ID = t.s.seq
	t.s.fixings = append(t.s.fixings, *f)
	return f.ID, nil
}

func (t *memTx) SetContractFixing(_ context.Context, id int64, rate decimal.Decimal) error {
	c, ok := t.s.contracts[id]
	if !ok {
		return excNotFound(id)
	}
	if c.Status != StatusOpen {
		return fmt.Errorf("%s: contract %d not OPEN", CodeDerivativeStateConflict, id)
	}
	c.FixingRate = &rate
	return nil
}

func (t *memTx) SetContractOutcome(_ context.Context, id int64, fixingRate, amount decimal.Decimal,
	st ContractStatus, settledAt time.Time) error {
	c, ok := t.s.contracts[id]
	if !ok {
		return excNotFound(id)
	}
	if c.Status != StatusOpen {
		return fmt.Errorf("%s: contract %d not OPEN", CodeDerivativeStateConflict, id)
	}
	c.FixingRate = &fixingRate
	c.SettlementAmount = &amount
	c.Status = st
	c.SettledAt = &settledAt
	return nil
}

func (t *memTx) DueContracts(_ context.Context, asOf time.Time, limit int) ([]Contract, error) {
	var out []Contract
	for _, c := range t.s.contracts {
		if c.Status != StatusOpen && c.Status != StatusPartiallySettled {
			continue
		}
		due := !c.ValueDate.After(asOf)
		if c.NearLegValueDate != nil && !c.NearLegValueDate.After(asOf) {
			due = true
		}
		if due {
			out = append(out, *c)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// setLegStatus flips a fake leg for rollup tests.
func (s *memStore) setLegStatus(contractID int64, legID int64, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.legs[contractID] {
		if s.legs[contractID][i].ID == legID {
			s.legs[contractID][i].Status = status
		}
	}
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	return excerrors.CodeOf(err)
}
