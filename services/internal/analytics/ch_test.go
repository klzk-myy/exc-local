// Live ClickHouse tests for Task 20.3.1 + 20.3.11 — gated on
// EXC_CH_TEST=1, dev cluster at 127.0.0.1:9000 (exchange/exchange_dev,
// db exchange_analytics). Reuses the chTestConn gate from pnl_test.go.
//
// Symbols are unique per run ("TST<unixnano>-…") so reruns never collide
// and no cleanup is needed — the 90-day tick TTL reclaims them.
package analytics

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func chCount(t *testing.T, conn Conn, query string, args ...any) uint64 {
	t.Helper()
	rows, err := conn.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("count query returned no rows")
	}
	var n uint64
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("count scan: %v", err)
	}
	return n
}

// TestCHDedupReplacingMergeTree (§16.6, §24 #322): the same
// (symbol, trade_id, event_seq) inserted twice with different ver
// collapses to exactly one row under FINAL.
func TestCHDedupReplacingMergeTree(t *testing.T) {
	conn := chTestConn(t)
	ctx := context.Background()
	sym := fmt.Sprintf("TST%d-DEDUP", time.Now().UnixNano()%1e9)

	ing := NewIngester(conn, nil, Options{InsertTimeout: 10 * time.Second}, nil, nil)
	row := TickRowValues(time.Now().UTC(), sym, 108501234, 1000, "UNKNOWN", 9001, 55, 0)
	// Same logical row, two ingest passes (replay) — each gets a new ver.
	for i := 0; i < 2; i++ {
		if err := ing.InsertWithSpool(ctx, "ticks", [][]any{row}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		time.Sleep(2 * time.Millisecond) // ensure distinct ver nanos
	}
	if got := chCount(t, conn,
		"SELECT count() FROM ticks WHERE symbol = ?", sym); got != 2 {
		t.Fatalf("raw rows = %d, want 2 (pre-merge duplicates)", got)
	}
	if got := chCount(t, conn,
		"SELECT count() FROM ticks FINAL WHERE symbol = ?", sym); got != 1 {
		t.Fatalf("FINAL rows = %d, want 1 — dedup invariant broken", got)
	}
}

// TestCHSpoolDrainLive: rows diverted to the spool while "CH is down"
// (fake conn) drain into the real cluster once a healthy conn is bound.
func TestCHSpoolDrainLive(t *testing.T) {
	conn := chTestConn(t)
	ctx := context.Background()
	dir := t.TempDir()
	s, err := OpenSpool(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sym := fmt.Sprintf("TST%d-DRAIN", time.Now().UnixNano()%1e9)

	// Phase 1: inserts fail -> spool.
	fc := newEtlFakeConn()
	fc.failInsert("ticks", true)
	down := NewIngester(fc, s, Options{InsertTimeout: time.Second}, nil, nil)
	row := TickRowValues(time.Now().UTC(), sym, 105, 2500, "UNKNOWN", 9002, 77, 0)
	if err := down.InsertWithSpool(ctx, "ticks", [][]any{row}); err != nil {
		t.Fatal(err)
	}
	if s.Entries() != 1 {
		t.Fatalf("spool entries = %d", s.Entries())
	}

	// Phase 2: healthy conn drains the spool.
	up := NewIngester(conn, s, Options{InsertTimeout: 10 * time.Second}, nil, nil)
	n, err := up.DrainOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || s.Entries() != 0 {
		t.Fatalf("drained = %d, entries = %d", n, s.Entries())
	}
	if got := chCount(t, conn,
		"SELECT count() FROM ticks FINAL WHERE symbol = ?", sym); got != 1 {
		t.Fatalf("drained rows = %d, want 1", got)
	}
}

// TestCHVolumeMV: the hourly rollup MV over ticks produces '1h' rows in
// volume_stats without any batch job.
func TestCHVolumeMV(t *testing.T) {
	conn := chTestConn(t)
	ctx := context.Background()
	sym := fmt.Sprintf("TST%d-MV", time.Now().UnixNano()%1e9)

	ing := NewIngester(conn, nil, Options{InsertTimeout: 10 * time.Second}, nil, nil)
	var rows [][]any
	for i := 0; i < 3; i++ {
		rows = append(rows, TickRowValues(time.Now().UTC(), sym,
			200000000+int64(i), 1000, "UNKNOWN", uint64(9100+i), uint64(i+1), 0))
	}
	if err := ing.InsertWithSpool(ctx, "ticks", rows); err != nil {
		t.Fatal(err)
	}
	// MV output merges asynchronously; FINAL forces the rollup for the read.
	got := chCount(t, conn,
		`SELECT sum(trade_count) FROM volume_stats FINAL
		 WHERE symbol = ? AND granularity = '1h'`, sym)
	if got != 3 {
		t.Fatalf("volume_stats trade_count = %d, want 3", got)
	}
}

// TestCHIncomeProjectionShape: income_ledger accepts the ETL's row shape
// and dedups replays of the same ledger_entry_id.
func TestCHIncomeProjectionShape(t *testing.T) {
	conn := chTestConn(t)
	ctx := context.Background()

	ing := NewIngester(conn, nil, Options{InsertTimeout: 10 * time.Second}, nil, nil)
	entryID := uint64(time.Now().UnixNano() % 1e12)
	row := []any{
		time.Now().UTC(), uint64(777001), "USD", "COMMISSION", "FEE",
		decimal.New(125, -2), entryID, uint64(88), uint64(42), "test fee",
	}
	for i := 0; i < 2; i++ {
		if err := ing.InsertWithSpool(ctx, "income_ledger", [][]any{row}); err != nil {
			t.Fatalf("income insert %d: %v", i, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	got := chCount(t, conn,
		`SELECT count() FROM income_ledger FINAL
		 WHERE account_id = 777001 AND ledger_entry_id = ?`, entryID)
	if got != 1 {
		t.Fatalf("income_ledger FINAL rows = %d, want 1 (replay dedup)", got)
	}
}

// TestCHIngestThroughput measures the columnar tick path at the §24 #65
// 50k-rows mark. The assert is a sanity bound (the whole run < 60s); the
// honest measurement goes to t.Log — containerized dev clusters vary.
func TestCHIngestThroughput(t *testing.T) {
	conn := chTestConn(t)
	ctx := context.Background()
	sym := fmt.Sprintf("TST%d-LOAD", time.Now().UnixNano()%1e9)

	ing := NewIngester(conn, nil, Options{InsertTimeout: 30 * time.Second}, nil, nil)

	const total = 50_000
	const perBatch = 10_000
	base := time.Now().UTC()
	start := time.Now()
	for off := 0; off < total; off += perBatch {
		rows := make([][]any, 0, perBatch)
		for i := off; i < off+perBatch; i++ {
			rows = append(rows, TickRowValues(base, sym,
				100000000+int64(i%1000), 1000, "UNKNOWN",
				uint64(1_000_000+i), uint64(i), 0))
		}
		if err := ing.InsertWithSpool(ctx, "ticks", rows); err != nil {
			t.Fatalf("batch at %d: %v", off, err)
		}
	}
	elapsed := time.Since(start)
	rate := float64(total) / elapsed.Seconds()
	t.Logf("ingested %d tick rows in %s = %.0f rows/sec (target 50000/sec)",
		total, elapsed, rate)
	if elapsed > 60*time.Second {
		t.Fatalf("ingest too slow: %s for %d rows (%.0f/s)", elapsed, total, rate)
	}
	if got := chCount(t, conn,
		"SELECT count() FROM ticks WHERE symbol = ?", sym); got != total {
		t.Fatalf("rows = %d, want %d", got, total)
	}
}
