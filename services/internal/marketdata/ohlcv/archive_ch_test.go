// Tests for CHArchiveSink — the Phase-20 Task 20.3.3 Candle→ClickHouse
// projection seam. Pure unit coverage over a fake CHSink; the live path
// is exercised by analytics' EXC_CH_TEST-gated tests.
package ohlcv

import (
	"context"
	"errors"
	"testing"
	"time"

	"exchange/internal/analytics"
	"exchange/pkg/decimal"
)

type fakeCHSink struct {
	calls [][]analytics.CandleRow
	err   error
}

func (f *fakeCHSink) Insert(_ context.Context, rows []analytics.CandleRow) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, rows)
	return nil
}

func TestCHArchiveSinkMapsCandle(t *testing.T) {
	sink := &fakeCHSink{}
	s := NewCHArchiveSink(sink)
	ot := time.Date(2026, 10, 6, 16, 0, 0, 0, time.UTC)
	c := Candle{
		InstrumentID: 7, Symbol: "EUR/USD", Interval: I4h,
		OpenTime: ot, CloseTime: I4h.Next(ot),
		Open:        decimal.MustFromString("1.0852"),
		High:        decimal.MustFromString("1.0901"),
		Low:         decimal.MustFromString("1.0849"),
		Close:       decimal.MustFromString("1.0897"),
		Volume:      decimal.MustFromString("123456.78"),
		QuoteVolume: decimal.MustFromString("134999.999"),
		TradeCount:  42,
		Closed:      true,
	}
	if err := s.Archive(context.Background(), c); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if len(sink.calls) != 1 || len(sink.calls[0]) != 1 {
		t.Fatalf("calls = %v", sink.calls)
	}
	r := sink.calls[0][0]
	if r.Symbol != "EUR/USD" || r.Interval != "4h" {
		t.Fatalf("row identity mismatch: %+v", r)
	}
	if !r.OpenTime.Equal(ot) {
		t.Fatalf("open_time = %v, want %v", r.OpenTime, ot)
	}
	if !r.High.Equal(decimal.MustFromString("1.0901")) ||
		!r.Volume.Equal(decimal.MustFromString("123456.78")) ||
		r.TradeCount != 42 {
		t.Fatalf("row values mismatch: %+v", r)
	}
	// The Interval label must route to a real projection table.
	if tbl, ok := analytics.OHLCVTableFor(r.Interval); !ok || tbl != "ohlcv_4h" {
		t.Fatalf("interval %q routed to %q (ok=%v)", r.Interval, tbl, ok)
	}
}

func TestCHArchiveSinkIgnoresOpenBars(t *testing.T) {
	sink := &fakeCHSink{}
	s := NewCHArchiveSink(sink)
	c := Candle{Symbol: "EUR/USD", Interval: I1m,
		OpenTime: time.Now().UTC(), Closed: false}
	if err := s.Archive(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("open bar must not reach CH: %v", sink.calls)
	}
}

// TestCHArchiveSinkAllPersistedIntervals pins the §16.2 contract: every
// one of the 12 persisted intervals produces a CandleRow whose Interval
// label resolves to a distinct ClickHouse table. 1s stays memory-only —
// the engine never hands it to Archive in production, but if it did the
// store boundary fails closed (interval has no table mapping).
func TestCHArchiveSinkAllPersistedIntervals(t *testing.T) {
	sink := &fakeCHSink{}
	s := NewCHArchiveSink(sink)
	ot := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	for _, iv := range PersistedIntervals {
		c := Candle{InstrumentID: 7, Symbol: "EUR/USD", Interval: iv,
			OpenTime: iv.Floor(ot), Closed: true}
		if err := s.Archive(context.Background(), c); err != nil {
			t.Fatalf("archive %s: %v", iv, err)
		}
	}
	if len(sink.calls) != len(PersistedIntervals) {
		t.Fatalf("calls = %d, want %d", len(sink.calls), len(PersistedIntervals))
	}
	for i, iv := range PersistedIntervals {
		r := sink.calls[i][0]
		if r.Interval != iv.String() {
			t.Fatalf("interval label = %q, want %q", r.Interval, iv.String())
		}
		if _, ok := analytics.OHLCVTableFor(r.Interval); !ok {
			t.Fatalf("interval %q has no CH table mapping", r.Interval)
		}
	}
	// CarryForward gap candles archive — they are real closed buckets.
	gap := gapCandle("EUR/USD", 7, I1m, ot, decimal.MustFromString("1.0852"))
	if err := s.Archive(context.Background(), gap); err != nil {
		t.Fatalf("gap candle archive: %v", err)
	}
	last := sink.calls[len(sink.calls)-1][0]
	if !last.Volume.IsZero() || last.TradeCount != 0 {
		t.Fatalf("gap candle should carry zero volume/trades: %+v", last)
	}
}

func TestCHArchiveSinkPropagatesError(t *testing.T) {
	boom := errors.New("ch down")
	s := NewCHArchiveSink(&fakeCHSink{err: boom})
	err := s.Archive(context.Background(), Candle{
		Symbol: "EUR/USD", Interval: I1m, Closed: true})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want sink error propagated", err)
	}
}
