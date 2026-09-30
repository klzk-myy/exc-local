// Unit + gated integration tests for the Task 20.3.3 OHLCV projection.
// Live tests reuse chTestConn from pnl_test.go (EXC_CH_TEST=1 gate).
package analytics

import (
	"context"
	"strings"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// TestOHLCVTableForAllIntervals pins the 12-timeframe projection contract
// (spec §16.2): every persisted label resolves to a distinct table; 1s
// (memory-only) and unknown labels fail closed.
func TestOHLCVTableForAllIntervals(t *testing.T) {
	seen := map[string]string{}
	for _, label := range PersistedIntervalLabels() {
		tbl, ok := OHLCVTableFor(label)
		if !ok {
			t.Fatalf("interval %q has no table mapping", label)
		}
		if dup, exists := seen[tbl]; exists {
			t.Fatalf("table %q mapped by both %q and %q", tbl, dup, label)
		}
		seen[tbl] = label
		if !strings.HasPrefix(tbl, "ohlcv_") {
			t.Fatalf("interval %q maps to unexpected table %q", label, tbl)
		}
	}
	if len(seen) != 12 {
		t.Fatalf("persisted set = %d tables, want 12", len(seen))
	}
	for _, bad := range []string{"1s", "2m", "1Y", "", " 1m", "1m "} {
		if tbl, ok := OHLCVTableFor(bad); ok {
			t.Fatalf("interval %q unexpectedly resolved to %q", bad, tbl)
		}
	}
	if PersistedInterval("1s") {
		t.Fatal("1s must stay memory-only")
	}
}

func TestOHLCVStoreInsertGroupsByInterval(t *testing.T) {
	conn := &tickFakeConn{}
	s := NewOHLCVStore(conn)
	ot := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	one := decimal.MustFromString("1.0852")
	rows := []CandleRow{
		{Symbol: "EUR/USD", Interval: "1m", OpenTime: ot,
			Open: one, High: one, Low: one, Close: one,
			Volume: decimal.MustFromString("100"), QuoteVolume: decimal.MustFromString("108.52"),
			TradeCount: 3},
		{Symbol: "EUR/USD", Interval: "1h", OpenTime: ot,
			Open: one, High: one, Low: one, Close: one,
			Volume: decimal.MustFromString("500"), QuoteVolume: decimal.MustFromString("542.6"),
			TradeCount: 11},
		{Symbol: "EUR/USD", Interval: "1m", OpenTime: ot.Add(time.Minute),
			Open: one, High: one, Low: one, Close: one,
			Volume: decimal.MustFromString("50"), QuoteVolume: decimal.MustFromString("54.26"),
			TradeCount: 2},
	}
	if err := s.Insert(context.Background(), rows); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// 3 rows across 2 intervals → 2 PrepareBatch calls.
	if len(conn.batchQueries()) != 2 {
		t.Fatalf("batches = %d, want 2: %v", len(conn.batchQueries()), conn.batchQueries())
	}
	var m1, h1 bool
	for _, b := range conn.batches {
		switch {
		case strings.Contains(b.query, "ohlcv_1m"):
			m1 = true
			if b.batch.Rows() != 2 {
				t.Fatalf("ohlcv_1m rows = %d, want 2", b.batch.Rows())
			}
		case strings.Contains(b.query, "ohlcv_1h"):
			h1 = true
			if b.batch.Rows() != 1 {
				t.Fatalf("ohlcv_1h rows = %d, want 1", b.batch.Rows())
			}
		default:
			t.Fatalf("unexpected batch target %q", b.query)
		}
		if !b.batch.sent {
			t.Fatalf("batch %q never sent", b.query)
		}
	}
	if !m1 || !h1 {
		t.Fatalf("missing per-interval batches: %v", conn.batchQueries())
	}
}

func TestOHLCVStoreInsertRejectsUnpersisted(t *testing.T) {
	conn := &tickFakeConn{}
	err := NewOHLCVStore(conn).Insert(context.Background(), []CandleRow{
		{Symbol: "EUR/USD", Interval: "1s", OpenTime: time.Now().UTC()},
	})
	if err == nil {
		t.Fatal("1s interval must be rejected — memory-only")
	}
	if len(conn.batches) != 0 {
		t.Fatal("no batch should be prepared for an invalid interval")
	}
}

func TestOHLCVStoreQuery(t *testing.T) {
	ot := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	one := decimal.MustFromString("1.0852")
	// Read projection order: bucket, symbol, open..close, volume,
	// quote_volume, trade_count.
	conn := &tickFakeConn{queryRows: [][]any{
		{ot, "EUR/USD", one, one, one, one,
			decimal.MustFromString("100"), decimal.MustFromString("108.52"), uint64(3)},
	}}
	s := NewOHLCVStore(conn)
	from, to := ot.Add(-time.Hour), ot.Add(time.Hour)
	got, err := s.Query(context.Background(), OHLCVQuery{
		Symbol: "EUR/USD", Interval: "1m", From: from, To: to, Limit: 500,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].TradeCount != 3 || !got[0].Close.Equal(one) {
		t.Fatalf("unexpected rows %+v", got)
	}
	q := conn.queries[0]
	for _, want := range []string{
		"FROM ohlcv_1m FINAL", "WHERE symbol = ?",
		"bucket >= ?", "bucket < ?", "ORDER BY bucket ASC LIMIT ?",
	} {
		if !strings.Contains(q, want) {
			t.Fatalf("query %q missing %q", q, want)
		}
	}
}

func TestOHLCVStoreQueryRejectsUnpersisted(t *testing.T) {
	_, err := NewOHLCVStore(&tickFakeConn{}).Query(context.Background(), OHLCVQuery{
		Symbol: "EUR/USD", Interval: "1s",
	})
	if err == nil {
		t.Fatal("1s query must fail closed")
	}
}

// ---------------------------------------------------------------------------
// Gated live-infra test.
// ---------------------------------------------------------------------------

func TestCHOhlcvStore(t *testing.T) {
	conn := chTestConn(t)
	ctx := context.Background()

	// The schema track ships twelve per-interval tables plus a UNION ALL
	// `ohlcv` view — inserts target the table, never the view.
	stmt, err := ShowCreateTable(ctx, conn, "ohlcv_1m")
	if err != nil {
		t.Skipf("ohlcv_1m not yet deployed (sibling DDL pending): %v", err)
	}
	// §16.2/§19.12: aggregated candles TTL 5 years.
	if !strings.Contains(stmt, "TTL") ||
		!(strings.Contains(stmt, "5 YEAR") || strings.Contains(stmt, "toIntervalYear(5)")) {
		t.Fatalf("ohlcv_1m DDL missing 5-year TTL: %s", stmt)
	}

	s := NewOHLCVStore(conn)
	ot := time.Now().UTC().Truncate(time.Minute).Add(-24 * time.Hour)
	one := decimal.MustFromString("9.99")
	row := CandleRow{
		Symbol: "TEST/PAIR", Interval: "1m", OpenTime: ot,
		Open: one, High: one, Low: one, Close: one,
		Volume: decimal.MustFromString("1"), QuoteVolume: decimal.MustFromString("9.99"),
		TradeCount: 1,
	}
	if err := s.Insert(ctx, []CandleRow{row}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.Query(ctx, OHLCVQuery{
		Symbol: "TEST/PAIR", Interval: "1m",
		// Narrow window: dev CH accumulates TEST/PAIR rows across runs —
		// a wide window + row limit clips the fresh insert unpredictably.
		From: ot, To: ot.Add(time.Minute), Limit: 10,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	found := false
	for _, g := range got {
		if g.OpenTime.Equal(ot) && g.Close.Equal(one) {
			found = true
		}
	}
	if !found {
		t.Fatalf("inserted candle not read back: %+v", got)
	}
}
