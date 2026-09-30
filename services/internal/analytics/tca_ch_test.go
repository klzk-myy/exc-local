// Gated live ClickHouse tests for Task 20.3.9 — EXC_CH_TEST=1, dev
// cluster 127.0.0.1:9000 (exchange/exchange_dev, exchange_analytics).
// Skips while the sibling-owned tca_results DDL has not been applied
// (ShowCreateTable probe, same convention as ticks_test.go).
package analytics

import (
	"context"
	"fmt"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// TestCHTCARoundTrip: insert a fill record via CHTCASink and read the
// bucketed aggregate back through CHReportStore — proves the sibling's
// tca_results column contract end-to-end.
func TestCHTCARoundTrip(t *testing.T) {
	conn := chTestConn(t)
	ctx := context.Background()
	if _, err := ShowCreateTable(ctx, conn, TCATable); err != nil {
		t.Skipf("tca_results DDL not yet deployed (sibling schema pending): %v", err)
	}
	sink := NewCHTCASink(conn)
	sym := fmt.Sprintf("TST%d-TCA", time.Now().UnixNano()%1e9)
	arrival := decimal.RequireFromString("1.0850")
	slip := decimal.RequireFromString("1.8435")
	rec := TCARecord{
		AccountID: 999999001, InstrumentID: 3, Symbol: sym,
		FillID: int64(time.Now().UnixNano() % 1e12), OrderID: 42,
		ExecPrice:    decimal.RequireFromString("1.0852"),
		ArrivalPrice: &arrival, SlipArrivalBps: &slip,
		Period: "fill", PeriodStart: time.Now().UTC(),
		Ts: time.Now().UTC(), Ver: uint64(time.Now().UnixNano()),
	}
	if err := sink.InsertTCA(ctx, rec); err != nil {
		t.Fatalf("insert: %v", err)
	}
	store := NewCHReportStore(conn)
	var rows []AggregateRow
	for i := 0; i < 20; i++ {
		var err error
		rows, err = store.Aggregate(ctx, ReportFilter{
			AccountID: rec.AccountID, Period: "daily",
			From: time.Now().UTC().Add(-time.Hour), To: time.Now().UTC().Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		for _, r := range rows {
			if r.Symbol == sym {
				goto found
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
found:
	ok := false
	for _, r := range rows {
		if r.Symbol == sym {
			ok = true
			if r.Fills != 1 {
				t.Fatalf("fills=%d", r.Fills)
			}
			if r.AvgSlipArrivalBps == nil || r.AvgSlipArrivalBps.Sub(slip).Abs().GreaterThan(dec("0.001")) {
				t.Fatalf("avg slip %v", r.AvgSlipArrivalBps)
			}
		}
	}
	if !ok {
		t.Fatalf("aggregate missed test symbol %s", sym)
	}
}

// TestCHTCAReplayDedup: re-inserting the same logical row under a higher
// ver collapses under FINAL — the at-least-once redelivery guarantee.
func TestCHTCAReplayDedup(t *testing.T) {
	conn := chTestConn(t)
	ctx := context.Background()
	if _, err := ShowCreateTable(ctx, conn, TCATable); err != nil {
		t.Skipf("tca_results DDL not yet deployed: %v", err)
	}
	sink := NewCHTCASink(conn)
	sym := fmt.Sprintf("TST%d-DUP", time.Now().UnixNano()%1e9)
	fillID := int64(time.Now().UnixNano() % 1e12)
	for i := 0; i < 2; i++ {
		rec := TCARecord{
			AccountID: 999999002, InstrumentID: 3, Symbol: sym,
			FillID: fillID, OrderID: 43,
			ExecPrice: decimal.RequireFromString("1.0852"),
			Period:    "fill", PeriodStart: time.Now().UTC(),
			Ts: time.Now().UTC(), Ver: uint64(time.Now().UnixNano()),
		}
		if err := sink.InsertTCA(ctx, rec); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	var n uint64
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := conn.Query(ctx,
			`SELECT count() FROM `+TCATable+` FINAL
			 WHERE symbol = ? AND fill_id = ?`, sym, fillID)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if rows.Next() {
			_ = rows.Scan(&n)
		}
		_ = rows.Close()
		if n == 1 {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("FINAL rows = %d, want 1 (replay dedup broken)", n)
}
