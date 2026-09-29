// Unit tests for Phase-13 Task 13.3.4 — real-time P&L service.
// Store/marks/converter/publisher are all faked; no DB or Redis needed.
package risk

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	"exchange/internal/position"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakePnlStore struct {
	rows    []PnlRow
	base    string
	rowsErr error
	baseErr error
	holders map[int64][]int64 // instrumentID → account ids
	holdErr error
}

func (f *fakePnlStore) PnlRows(context.Context, int64) ([]PnlRow, error) {
	if f.rowsErr != nil {
		return nil, f.rowsErr
	}
	return f.rows, nil
}

func (f *fakePnlStore) AccountBaseCurrency(context.Context, int64) (string, error) {
	if f.baseErr != nil {
		return "", f.baseErr
	}
	return f.base, nil
}

func (f *fakePnlStore) AccountsHoldingInstrument(_ context.Context, id int64) ([]int64, error) {
	if f.holdErr != nil {
		return nil, f.holdErr
	}
	return f.holders[id], nil
}

type fakeMarks struct {
	px  map[int64]*decimal.Decimal
	err error
}

func (f *fakeMarks) ReferencePrice(_ context.Context, id int64) (*decimal.Decimal, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.px[id], nil
}

type fakeConv struct {
	rate decimal.Decimal
	err  error
}

func (f fakeConv) Convert(_ context.Context, amount decimal.Decimal, from, to string) (position.Conversion, error) {
	if f.err != nil {
		return position.Conversion{}, f.err
	}
	return position.Conversion{
		FromCurrency: from, ToCurrency: to,
		FromAmount: amount, ToAmount: amount.Mul(f.rate),
		Rate: f.rate, Path: from + "/" + to,
	}, nil
}

type pushCall struct {
	account int64
	channel string
	data    any
}

type fakePush struct{ calls []pushCall }

func (f *fakePush) PublishPrivate(accountID int64, channel string, data any) {
	f.calls = append(f.calls, pushCall{accountID, channel, data})
}

// d() is already declared in circuit_breaker_test.go (same package).
func dptr(s string) *decimal.Decimal { v := d(s); return &v }

// ---------------------------------------------------------------------------
// Snapshot math
// ---------------------------------------------------------------------------

func TestPnlSnapshotLongAndShort(t *testing.T) {
	// LONG 10 EUR/USD @1.10 marked 1.20 → uPnL +1.00 USD.
	// SHORT 5 EUR/USD @1.30 marked 1.20 → uPnL +0.50 USD.
	store := &fakePnlStore{base: "USD", rows: []PnlRow{
		{InstrumentID: 1, Symbol: "EUR/USD", QuoteCurrency: "USD",
			Side: "LONG", Quantity: d("10"), EntryPrice: d("1.10"),
			RealizedPnL: d("2.00")},
		{InstrumentID: 1, Symbol: "EUR/USD", QuoteCurrency: "USD",
			Side: "SHORT", Quantity: d("5"), EntryPrice: d("1.30"),
			RealizedPnL: d("-0.25")},
	}}
	marks := &fakeMarks{px: map[int64]*decimal.Decimal{1: dptr("1.20")}}
	svc, err := NewPnlService(PnlOptions{Store: store, Marks: marks,
		Now: func() time.Time { return time.UnixMilli(1_700_000_000_000) }})
	if err != nil {
		t.Fatal(err)
	}
	v, err := svc.Snapshot(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if v.Event != "pnl" || v.AccountID != 7 || v.BaseCurrency != "USD" {
		t.Fatalf("view header: %+v", v)
	}
	if len(v.Positions) != 2 {
		t.Fatalf("positions: %+v", v.Positions)
	}
	if v.Positions[0].UnrealizedPnL != "1" || v.Positions[0].MarkSource != "last_trade" {
		t.Fatalf("long uPnL: %+v", v.Positions[0])
	}
	if v.Positions[1].UnrealizedPnL != "0.5" {
		t.Fatalf("short uPnL: %+v", v.Positions[1])
	}
	if v.Converted {
		t.Fatal("no converter bound — converted must be false")
	}
	if len(v.ByCurrency) != 1 || v.ByCurrency[0].Currency != "USD" ||
		v.ByCurrency[0].UnrealizedPnL != "1.5" ||
		v.ByCurrency[0].RealizedPnL != "1.75" ||
		v.ByCurrency[0].TotalPnL != "3.25" {
		t.Fatalf("by_currency: %+v", v.ByCurrency)
	}
}

func TestPnlMarkFallbackChain(t *testing.T) {
	// No oracle price → stored mark; no stored mark → entry (uPnL 0).
	store := &fakePnlStore{base: "USD", rows: []PnlRow{
		{InstrumentID: 1, Symbol: "A/B", QuoteCurrency: "USD",
			Side: "LONG", Quantity: d("1"), EntryPrice: d("2.00"),
			StoredMark: dptr("2.50")},
		{InstrumentID: 2, Symbol: "C/D", QuoteCurrency: "USD",
			Side: "LONG", Quantity: d("1"), EntryPrice: d("3.00")},
	}}
	marks := &fakeMarks{px: map[int64]*decimal.Decimal{}}
	svc, err := NewPnlService(PnlOptions{Store: store, Marks: marks})
	if err != nil {
		t.Fatal(err)
	}
	v, err := svc.Snapshot(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if v.Positions[0].MarkSource != "stored_mark" || v.Positions[0].MarkPrice != "2.5" {
		t.Fatalf("stored-mark fallback: %+v", v.Positions[0])
	}
	if v.Positions[1].MarkSource != "entry" || v.Positions[1].UnrealizedPnL != "0" {
		t.Fatalf("entry fallback: %+v", v.Positions[1])
	}
}

func TestPnlMarkLookupFailsClosed(t *testing.T) {
	store := &fakePnlStore{base: "USD", rows: []PnlRow{
		{InstrumentID: 1, Symbol: "EUR/USD", QuoteCurrency: "USD",
			Side: "LONG", Quantity: d("1"), EntryPrice: d("1")},
	}}
	marks := &fakeMarks{err: stderrors.New("trades store down")}
	svc, err := NewPnlService(PnlOptions{Store: store, Marks: marks})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Snapshot(context.Background(), 7); err == nil {
		t.Fatal("mark lookup failure must abort the snapshot (fail closed)")
	}
}

func TestPnlFailClosedOnStore(t *testing.T) {
	svc, err := NewPnlService(PnlOptions{
		Store: &fakePnlStore{baseErr: stderrors.New("pg down")},
		Marks: &fakeMarks{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Snapshot(context.Background(), 7); err == nil {
		t.Fatal("store failure must surface, not fabricate zero P&L")
	}
	// Unknown account propagates the coded ACCOUNT_NOT_FOUND.
	svc2, _ := NewPnlService(PnlOptions{
		Store: &fakePnlStore{
			baseErr: excerrors.New("ACCOUNT_NOT_FOUND", "missing")},
		Marks: &fakeMarks{}})
	_, err = svc2.Snapshot(context.Background(), 99)
	var ce *excerrors.Error
	if !stderrors.As(err, &ce) || ce.Code != "ACCOUNT_NOT_FOUND" {
		t.Fatalf("want coded ACCOUNT_NOT_FOUND, got %v", err)
	}
}

func TestPnlBaseCurrencyConversion(t *testing.T) {
	// One USD-quoted + one JPY-quoted position, base USD, converter at
	// 0.0067 USD per JPY → converted totals populated.
	store := &fakePnlStore{base: "USD", rows: []PnlRow{
		{InstrumentID: 1, Symbol: "EUR/USD", QuoteCurrency: "USD",
			Side: "LONG", Quantity: d("10"), EntryPrice: d("1.10")},
		{InstrumentID: 2, Symbol: "USD/JPY", QuoteCurrency: "JPY",
			Side: "LONG", Quantity: d("100"), EntryPrice: d("150")},
	}}
	marks := &fakeMarks{px: map[int64]*decimal.Decimal{
		1: dptr("1.20"), 2: dptr("160")}}
	svc, err := NewPnlService(PnlOptions{Store: store, Marks: marks,
		Converter: fakeConv{rate: d("0.01")}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := svc.Snapshot(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Converted || v.TotalPnL == nil {
		t.Fatalf("conversion expected: %+v", v)
	}
	// USD leg: 10×(1.20−1.10)=1.00; JPY leg: 100×(160−150)=1000 → ×0.01=10.
	if *v.UnrealizedPnL != "11" || *v.TotalPnL != "11" {
		t.Fatalf("totals: %+v", v)
	}
	// Conversion failure aborts — never a half-converted total.
	svc2, _ := NewPnlService(PnlOptions{Store: store, Marks: marks,
		Converter: fakeConv{err: stderrors.New("oracle down")}})
	if _, err := svc2.Snapshot(context.Background(), 7); err == nil {
		t.Fatal("converter failure must abort the snapshot (fail closed)")
	}
}

// ---------------------------------------------------------------------------
// Push path
// ---------------------------------------------------------------------------

func TestPnlOnTradePublishesAccountAndHolders(t *testing.T) {
	store := &fakePnlStore{base: "USD",
		rows: []PnlRow{{InstrumentID: 1, Symbol: "EUR/USD",
			QuoteCurrency: "USD", Side: "LONG", Quantity: d("1"),
			EntryPrice: d("1.10")}},
		holders: map[int64][]int64{1: {7, 8}}}
	marks := &fakeMarks{px: map[int64]*decimal.Decimal{1: dptr("1.20")}}
	push := &fakePush{}
	svc, err := NewPnlService(PnlOptions{Store: store, Marks: marks, Publisher: push})
	if err != nil {
		t.Fatal(err)
	}
	svc.OnTrade(context.Background(), 7, 1)
	// Account 7 (the fill) + holders 7 & 8 (mark update fanout).
	got := map[int64]int{}
	for _, c := range push.calls {
		if c.channel != PnlChannel {
			t.Fatalf("channel %q, want %q", c.channel, PnlChannel)
		}
		got[c.account]++
	}
	if got[7] != 2 || got[8] != 1 {
		t.Fatalf("fanout: %v", got)
	}
	// Payload is the same PnlView the REST endpoint serves.
	pv, ok := push.calls[0].data.(*PnlView)
	if !ok || pv.Event != "pnl" {
		t.Fatalf("payload: %#v", push.calls[0].data)
	}
}
