// Unit + gated integration tests for the Task 20.3.2 tick store.
//
// Unit tests run everywhere against in-memory driver fakes. The
// TestCH* tests are live-infrastructure gated: they dial the dev
// ClickHouse (EXC_CH_* env or 127.0.0.1:9000 exchange/exchange_dev,
// db exchange_analytics) and SKIP unless EXC_CH_TEST=1. When the
// sibling-owned schema has not landed yet they skip again with a note —
// the DDL assertions are ready for the moment the tables exist.
package analytics

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes — minimal driver.Batch / driver.Rows / Conn stand-ins.
// ---------------------------------------------------------------------------

type tickFakeBatch struct {
	rows      [][]any
	sent      bool
	aborted   bool
	appendErr error
	sendErr   error
}

func (b *tickFakeBatch) Abort() error { b.aborted = true; return nil }

func (b *tickFakeBatch) Append(v ...any) error {
	if b.appendErr != nil {
		return b.appendErr
	}
	b.rows = append(b.rows, v)
	return nil
}

func (b *tickFakeBatch) AppendStruct(any) error        { return fmt.Errorf("unimplemented") }
func (b *tickFakeBatch) Column(int) driver.BatchColumn { return nil }
func (b *tickFakeBatch) Flush() error                  { return nil }
func (b *tickFakeBatch) Send() error                   { b.sent = true; return b.sendErr }
func (b *tickFakeBatch) IsSent() bool                  { return b.sent }
func (b *tickFakeBatch) Rows() int                     { return len(b.rows) }
func (b *tickFakeBatch) Columns() []column.Interface   { return nil }
func (b *tickFakeBatch) Close() error                  { return nil }

type tickFakeRows struct {
	data [][]any
	idx  int
	err  error
}

func (r *tickFakeRows) Next() bool { r.idx++; return r.idx <= len(r.data) }

func (r *tickFakeRows) Scan(dest ...any) error {
	row := r.data[r.idx-1]
	if len(dest) != len(row) {
		return fmt.Errorf("scan arity %d != %d", len(dest), len(row))
	}
	for i := range dest {
		if err := assignTickDest(dest[i], row[i]); err != nil {
			return err
		}
	}
	return nil
}

func assignTickDest(dest, v any) error {
	switch d := dest.(type) {
	case *int64:
		*d = v.(int64)
	case *uint64:
		*d = v.(uint64)
	case *uint32:
		*d = v.(uint32)
	case *string:
		*d = v.(string)
	case *time.Time:
		*d = v.(time.Time)
	case *decimal.Decimal:
		*d = v.(decimal.Decimal)
	default:
		return fmt.Errorf("assign %T <- %T", dest, v)
	}
	return nil
}

func (r *tickFakeRows) ScanStruct(any) error             { return fmt.Errorf("unimplemented") }
func (r *tickFakeRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *tickFakeRows) Totals(...any) error              { return nil }
func (r *tickFakeRows) Columns() []string                { return nil }
func (r *tickFakeRows) Close() error                     { return nil }
func (r *tickFakeRows) Err() error                       { return r.err }

// preparedTickBatch couples a tickFakeBatch with the INSERT statement that made
// it — multi-table inserts (OHLCV per-interval grouping) need all of
// them, not just the last.
type preparedTickBatch struct {
	query string
	batch *tickFakeBatch
}

type tickFakeConn struct {
	batches    []preparedTickBatch
	prepareErr error

	queries   []string
	queryArgs [][]any
	queryRows [][]any
	queryErr  error
}

// batch/batchQuery are the last-prepared convenience accessors for
// single-batch tests.
func (c *tickFakeConn) lastBatch() *preparedTickBatch {
	if len(c.batches) == 0 {
		return nil
	}
	return &c.batches[len(c.batches)-1]
}

func (c *tickFakeConn) batchQueries() []string {
	out := make([]string, len(c.batches))
	for i := range c.batches {
		out[i] = c.batches[i].query
	}
	return out
}

func (c *tickFakeConn) Exec(context.Context, string, ...any) error { return nil }

func (c *tickFakeConn) Query(_ context.Context, q string, args ...any) (driver.Rows, error) {
	c.queries = append(c.queries, q)
	c.queryArgs = append(c.queryArgs, args)
	if c.queryErr != nil {
		return nil, c.queryErr
	}
	return &tickFakeRows{data: c.queryRows}, nil
}

func (c *tickFakeConn) PrepareBatch(_ context.Context, q string,
	_ ...driver.PrepareBatchOption) (driver.Batch, error) {
	if c.prepareErr != nil {
		return nil, c.prepareErr
	}
	b := &tickFakeBatch{}
	c.batches = append(c.batches, preparedTickBatch{query: q, batch: b})
	return b, nil
}

func (c *tickFakeConn) Ping(context.Context) error { return nil }
func (c *tickFakeConn) Close() error               { return nil }

// ---------------------------------------------------------------------------
// Unit tests
// ---------------------------------------------------------------------------

func TestTickStoreInsert(t *testing.T) {
	conn := &tickFakeConn{}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s := NewTickStore(conn)
	s.Now = func() time.Time { return now }

	ts := now.Add(-time.Second)
	err := s.Insert(context.Background(), []Tick{
		{ShardID: 2, Symbol: "EUR/USD", TradeID: 1001, EventSeq: 42,
			Price: decimal.MustFromString("1.08521000"),
			Qty:   decimal.MustFromString("100000"), Side: "BUY", Ts: ts},
		{ShardID: 2, Symbol: "EUR/USD", TradeID: 1002, EventSeq: 43,
			Price: decimal.MustFromString("1.08522000"),
			Qty:   decimal.MustFromString("50000"), Side: "SELL", Ts: ts.Add(time.Millisecond)},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	pb := conn.lastBatch()
	if pb == nil || !pb.batch.sent {
		t.Fatal("batch not sent")
	}
	if !strings.HasPrefix(pb.query, "INSERT INTO "+TickTable+" (") {
		t.Fatalf("unexpected insert query %q", pb.query)
	}
	if got := pb.batch.Rows(); got != 2 {
		t.Fatalf("batch rows = %d, want 2", got)
	}
	row := pb.batch.rows[0]
	// Column order: ts, symbol, price, quantity, side, trade_id,
	// event_seq, shard_id, ver.
	if row[5].(uint64) != 1001 || row[0].(time.Time) != ts ||
		row[7].(uint32) != 2 {
		t.Fatalf("row[0] trade_id/ts/shard mismatch: %v", row)
	}
	// ver = ingest millis (injected clock).
	if row[8].(uint64) != uint64(now.UnixMilli()) {
		t.Fatalf("ver = %v, want ingest millis", row[8])
	}
}

func TestTickStoreInsertEmptyIsNoop(t *testing.T) {
	conn := &tickFakeConn{}
	if err := NewTickStore(conn).Insert(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(conn.batches) != 0 {
		t.Fatal("empty insert must not prepare a batch")
	}
}

func TestTickStoreQuery(t *testing.T) {
	ts1 := time.Date(2026, 10, 6, 11, 59, 0, 0, time.UTC)
	ts2 := ts1.Add(-time.Second)
	// Read projection order: ts, symbol, price, quantity, side,
	// trade_id, event_seq, shard_id.
	conn := &tickFakeConn{queryRows: [][]any{
		{ts1, "EUR/USD", decimal.MustFromString("1.08522"),
			decimal.MustFromString("50000"), "SELL",
			uint64(1002), uint64(43), uint32(2)},
		{ts2, "EUR/USD", decimal.MustFromString("1.08521"),
			decimal.MustFromString("100000"), "BUY",
			uint64(1001), uint64(42), uint32(2)},
	}}
	s := NewTickStore(conn)
	from := ts2.Add(-time.Hour)
	to := ts1.Add(time.Hour)

	got, err := s.Query(context.Background(), TickQuery{
		Symbol: "EUR/USD", From: from, To: to, Limit: 10,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 2 || got[0].TradeID != 1002 || got[1].TradeID != 1001 {
		t.Fatalf("unexpected ticks %+v", got)
	}
	if !got[0].Price.Equal(decimal.MustFromString("1.08522")) {
		t.Fatalf("price = %v", got[0].Price)
	}
	q := conn.queries[0]
	for _, want := range []string{
		"FROM " + TickTable + " FINAL", "WHERE symbol = ?",
		"ts >= ?", "ts < ?", "ORDER BY ts DESC, trade_id DESC LIMIT ?",
	} {
		if !strings.Contains(q, want) {
			t.Fatalf("query %q missing %q", q, want)
		}
	}
	args := conn.queryArgs[0]
	if len(args) != 4 || args[0] != "EUR/USD" || args[3] != 10 {
		t.Fatalf("args = %v", args)
	}
}

func TestTickStoreQueryCursor(t *testing.T) {
	conn := &tickFakeConn{}
	cur := &TickCursor{Ts: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC), TradeID: 999}
	_, err := NewTickStore(conn).Query(context.Background(), TickQuery{
		Symbol: "USD/JPY", After: cur,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conn.queries[0], "(ts, trade_id) < (?, ?)") {
		t.Fatalf("keyset clause missing: %q", conn.queries[0])
	}
	args := conn.queryArgs[0]
	if args[1].(time.Time) != cur.Ts || args[2].(uint64) != uint64(999) {
		t.Fatalf("cursor args = %v", args)
	}
}

func TestShowCreateTable(t *testing.T) {
	conn := &tickFakeConn{queryRows: [][]any{
		{"CREATE TABLE exchange_analytics.ticks ... TTL ts + INTERVAL 90 DAY"},
	}}
	stmt, err := ShowCreateTable(context.Background(), conn, TickTable)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stmt, "TTL") {
		t.Fatalf("statement = %q", stmt)
	}
}

// ---------------------------------------------------------------------------
// Gated live-infra test: insert/select + schema contract assertions.
// Reuses the sibling's chTestConn gate (pnl_test.go): EXC_CH_TEST=1,
// dev ClickHouse at 127.0.0.1:9000 exchange/exchange_dev,
// db exchange_analytics.
// ---------------------------------------------------------------------------

func TestCHTickStore(t *testing.T) {
	conn := chTestConn(t)
	ctx := context.Background()

	stmt, err := ShowCreateTable(ctx, conn, TickTable)
	if err != nil {
		t.Skipf("ticks table not yet deployed (sibling DDL pending): %v", err)
	}
	// §16.1: raw ticks TTL 90 days. The DDL may render it as
	// `INTERVAL 90 DAY` or `toIntervalDay(90)` — accept either shape.
	if !strings.Contains(stmt, "TTL") ||
		!(strings.Contains(stmt, "90 DAY") || strings.Contains(stmt, "toIntervalDay(90)") ||
			strings.Contains(stmt, "toIntervalDay( 90 )")) {
		t.Fatalf("ticks DDL missing 90-day TTL: %s", stmt)
	}
	// LZ4 compression: MergeTree's default codec. An explicit non-default
	// CODEC( on the price/qty columns would appear in the statement;
	// absence means LZ4 per the ClickHouse default.
	if strings.Contains(stmt, "CODEC(") && !strings.Contains(stmt, "LZ4") {
		t.Fatalf("ticks DDL sets a non-default codec (LZ4 expected): %s", stmt)
	}

	s := NewTickStore(conn)
	ts := time.Now().UTC().Truncate(time.Millisecond)
	tk := Tick{
		ShardID: 0, Symbol: "TEST/PAIR", TradeID: 1<<63 - 1,
		EventSeq: 1, Price: decimal.MustFromString("9.99"),
		Qty: decimal.MustFromString("1"), Side: "BUY", Ts: ts,
	}
	if err := s.Insert(ctx, []Tick{tk}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.Query(ctx, TickQuery{
		Symbol: "TEST/PAIR", From: ts.Add(-time.Minute), To: ts.Add(time.Minute),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	found := false
	for _, g := range got {
		if g.TradeID == tk.TradeID && g.Price.Equal(tk.Price) {
			found = true
		}
	}
	if !found {
		t.Fatalf("inserted tick not read back: %+v", got)
	}
}
