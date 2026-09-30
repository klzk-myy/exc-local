// Unit tests for the Task 20.3.4 P&L store — in-memory driver fakes (no
// ClickHouse needed) plus the EXC_CH_TEST=1 gated live round-trip.
//
// This file owns two shared test seams for the whole package:
//   - chTestConn — the EXC_CH_TEST=1 live-gate helper reused by every
//     sibling TestCH* test (ch_test.go, ohlcv_test.go, ticks_test.go).
//   - pnlFakeConn / pnlFakeRows / pnlFakeBatch — the driver.Conn
//     stand-ins for the pnl/stats unit tests (tickFake* in ticks_test.go
//     are namespaced to the tick store; these serve 20.3.4/.5).
package analytics

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Shared fakes — driver.Batch / driver.Rows / Conn stand-ins.
// ---------------------------------------------------------------------------

type pnlFakeBatch struct {
	rows      [][]any
	sent      bool
	aborted   bool
	appendErr error
	sendErr   error
}

func (b *pnlFakeBatch) Abort() error { b.aborted = true; return nil }

func (b *pnlFakeBatch) Append(v ...any) error {
	if b.appendErr != nil {
		return b.appendErr
	}
	b.rows = append(b.rows, v)
	return nil
}

func (b *pnlFakeBatch) AppendStruct(any) error        { return fmt.Errorf("unimplemented") }
func (b *pnlFakeBatch) Column(int) driver.BatchColumn { return nil }
func (b *pnlFakeBatch) Flush() error                  { return nil }
func (b *pnlFakeBatch) Send() error                   { b.sent = true; return b.sendErr }
func (b *pnlFakeBatch) IsSent() bool                  { return b.sent }
func (b *pnlFakeBatch) Rows() int                     { return len(b.rows) }
func (b *pnlFakeBatch) Columns() []column.Interface   { return nil }
func (b *pnlFakeBatch) Close() error                  { return nil }

type pnlFakeRows struct {
	data [][]any
	idx  int
	err  error
}

func (r *pnlFakeRows) Next() bool { r.idx++; return r.idx <= len(r.data) }

func (r *pnlFakeRows) Scan(dest ...any) error {
	row := r.data[r.idx-1]
	if len(dest) != len(row) {
		return fmt.Errorf("scan arity %d != %d", len(dest), len(row))
	}
	for i := range dest {
		if err := pnlAssignDest(dest[i], row[i]); err != nil {
			return err
		}
	}
	return nil
}

// pnlAssignDest covers the concrete destination types the pnl/stats
// stores scan into (matching the ClickHouse column types in
// deploy/clickhouse/schema/).
func pnlAssignDest(dest, v any) error {
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

func (r *pnlFakeRows) ScanStruct(any) error             { return fmt.Errorf("unimplemented") }
func (r *pnlFakeRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *pnlFakeRows) Totals(...any) error              { return nil }
func (r *pnlFakeRows) Columns() []string                { return nil }
func (r *pnlFakeRows) Close() error                     { return nil }
func (r *pnlFakeRows) Err() error                       { return r.err }

// preparedPnLBatch couples a pnlFakeBatch with the INSERT statement that
// produced it.
type preparedPnLBatch struct {
	query string
	batch *pnlFakeBatch
}

type pnlFakeConn struct {
	batches    []preparedPnLBatch
	prepareErr error

	queries   []string
	queryArgs [][]any
	queryRows [][]any
	queryErr  error
}

func (c *pnlFakeConn) lastBatch() *preparedPnLBatch {
	if len(c.batches) == 0 {
		return nil
	}
	return &c.batches[len(c.batches)-1]
}

func (c *pnlFakeConn) Exec(context.Context, string, ...any) error { return nil }

func (c *pnlFakeConn) Query(_ context.Context, q string, args ...any) (driver.Rows, error) {
	c.queries = append(c.queries, q)
	c.queryArgs = append(c.queryArgs, args)
	if c.queryErr != nil {
		return nil, c.queryErr
	}
	return &pnlFakeRows{data: c.queryRows}, nil
}

func (c *pnlFakeConn) PrepareBatch(_ context.Context, q string,
	_ ...driver.PrepareBatchOption) (driver.Batch, error) {
	if c.prepareErr != nil {
		return nil, c.prepareErr
	}
	b := &pnlFakeBatch{}
	c.batches = append(c.batches, preparedPnLBatch{query: q, batch: b})
	return b, nil
}

func (c *pnlFakeConn) Ping(context.Context) error { return nil }
func (c *pnlFakeConn) Close() error               { return nil }

// ---------------------------------------------------------------------------
// chTestConn — the canonical live-gate helper for the analytics package's
// TestCH* tests (reused by ch_test.go / ohlcv_test.go / ticks_test.go).
// Dials the dev ClickHouse (native :9000; EXC_CH_* env or
// exchange/exchange_dev @ exchange_analytics) and SKIPS unless
// EXC_CH_TEST=1; an unreachable cluster skips too — dev-box absence is
// not a code failure.
// ---------------------------------------------------------------------------

func chTestConn(t *testing.T) Conn {
	t.Helper()
	if os.Getenv("EXC_CH_TEST") != "1" {
		t.Skip("EXC_CH_TEST=1 not set — skipping live ClickHouse test")
	}
	cfg := ConfigFromEnv()
	if cfg.User == "" {
		cfg.User = "exchange"
	}
	if cfg.Password == "" {
		cfg.Password = "exchange_dev"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := Dial(ctx, cfg)
	if err != nil {
		t.Skipf("ClickHouse unreachable at %s: %v", cfg.Addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

var pnlFixedNow = time.Date(2026, 9, 20, 15, 4, 5, 123456789, time.UTC)

func TestPnLUpsertBatchesRows(t *testing.T) {
	conn := &pnlFakeConn{}
	s := NewPnLStore(conn)
	s.Now = func() time.Time { return pnlFixedNow }
	err := s.Upsert(context.Background(),
		PnLRow{AccountID: 7, InstrumentID: 9, Symbol: "EUR/USD",
			Day:        time.Date(2026, 9, 19, 23, 30, 0, 0, time.UTC), // must truncate to Date
			Realized:   decimal.RequireFromString("10.5"),
			Unrealized: decimal.RequireFromString("-2.25"),
			Fees:       decimal.RequireFromString("0.75")},
		PnLRow{AccountID: 7, InstrumentID: 11, Symbol: "USD/JPY",
			Day:        time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC),
			Realized:   decimal.RequireFromString("3"),
			Unrealized: decimal.Zero,
			Fees:       decimal.RequireFromString("0.1")})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	pb := conn.lastBatch()
	if pb == nil || !pb.batch.sent {
		t.Fatal("batch not sent")
	}
	if !strings.HasPrefix(pb.query, "INSERT INTO "+TableAccountPnL+" (") {
		t.Fatalf("unexpected insert query %q", pb.query)
	}
	for _, col := range []string{"account_id", "instrument_id", "symbol", "day",
		"realized", "unrealized", "fees", "ts", "ver"} {
		if !strings.Contains(pb.query, col) {
			t.Fatalf("insert missing column %q: %s", col, pb.query)
		}
	}
	if got := pb.batch.Rows(); got != 2 {
		t.Fatalf("batch rows = %d, want 2", got)
	}
	row := pb.batch.rows[0]
	// Column order: account_id, instrument_id, symbol, day, realized,
	// unrealized, fees, ts, ver.
	if row[0] != int64(7) || row[1] != int64(9) || row[2] != "EUR/USD" {
		t.Fatalf("row keys = %v", row[:4])
	}
	if d := row[3].(time.Time); !d.Equal(time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("day not truncated: %v", d)
	}
	if !row[4].(decimal.Decimal).Equal(decimal.RequireFromString("10.5")) ||
		!row[5].(decimal.Decimal).Equal(decimal.RequireFromString("-2.25")) ||
		!row[6].(decimal.Decimal).Equal(decimal.RequireFromString("0.75")) {
		t.Fatalf("money columns = %v", row[4:7])
	}
	if row[8] != uint64(pnlFixedNow.UnixNano()) {
		t.Fatalf("ver = %v, want %d", row[8], pnlFixedNow.UnixNano())
	}
}

func TestPnLUpsertVersionMonotonicAcrossCalls(t *testing.T) {
	conn := &pnlFakeConn{}
	clock := pnlFixedNow
	s := NewPnLStore(conn)
	s.Now = func() time.Time { return clock }
	if err := s.Upsert(context.Background(),
		PnLRow{AccountID: 1, Day: pnlFixedNow}); err != nil {
		t.Fatalf("upsert1: %v", err)
	}
	clock = clock.Add(time.Second) // later write → strictly newer version
	if err := s.Upsert(context.Background(),
		PnLRow{AccountID: 1, Day: pnlFixedNow}); err != nil {
		t.Fatalf("upsert2: %v", err)
	}
	v1 := conn.batches[0].batch.rows[0][8].(uint64)
	v2 := conn.batches[1].batch.rows[0][8].(uint64)
	if v2 <= v1 {
		t.Fatalf("versions not monotonic: %d then %d", v1, v2)
	}
}

func TestPnLUpsertEmptyAndNilConn(t *testing.T) {
	conn := &pnlFakeConn{}
	if err := NewPnLStore(conn).Upsert(context.Background()); err != nil {
		t.Fatalf("empty upsert must be a no-op: %v", err)
	}
	if len(conn.batches) != 0 {
		t.Fatal("batch prepared for empty upsert")
	}
	if err := NewPnLStore(nil).Upsert(context.Background(), PnLRow{}); err == nil {
		t.Fatal("nil conn must fail closed")
	}
}

func TestPnLUpsertSendErrorPropagates(t *testing.T) {
	conn := &pnlFakeConn{}
	err := NewPnLStore(conn).Upsert(context.Background(),
		PnLRow{AccountID: 1, Day: pnlFixedNow})
	if err != nil {
		t.Fatalf("clean upsert failed: %v", err)
	}
	conn.batches[0].batch.sendErr = errSentinel{}
	if err := conn.batches[0].batch.Send(); err == nil {
		t.Fatal("sendErr must surface")
	}
	// PrepareBatch failure path.
	bad := &pnlFakeConn{prepareErr: errSentinel{}}
	if err := NewPnLStore(bad).Upsert(context.Background(),
		PnLRow{AccountID: 1, Day: pnlFixedNow}); err == nil {
		t.Fatal("prepare error must propagate")
	}
}

type errSentinel struct{}

func (errSentinel) Error() string { return "sentinel" }

func TestPnLReportAggregationQuery(t *testing.T) {
	conn := &pnlFakeConn{queryRows: [][]any{
		// (account_id, instrument_id, symbol, day, realized, unrealized,
		//  fees, ts, ver) — concrete types matching pnlAssignDest.
		{int64(42), int64(9), "EUR/USD",
			time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC),
			decimal.RequireFromString("10.5"),
			decimal.RequireFromString("-2.25"),
			decimal.RequireFromString("0.75"),
			time.Date(2026, 9, 19, 23, 0, 0, 0, time.UTC), uint64(100)},
		{int64(42), int64(9), "EUR/USD",
			time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
			decimal.RequireFromString("1"),
			decimal.Zero, decimal.Zero,
			time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC), uint64(101)},
	}}
	s := NewPnLStore(conn)
	from := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	rows, err := s.Report(context.Background(), 42, from, to)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	q := conn.queries[0]
	for _, frag := range []string{
		TableAccountPnL + " FINAL", "sum(realized)", "sum(fees)",
		"GROUP BY account_id, instrument_id, symbol, day",
		"ORDER BY day ASC",
	} {
		if !strings.Contains(q, frag) {
			t.Fatalf("query missing %q: %s", frag, q)
		}
	}
	args := conn.queryArgs[0]
	if args[0] != int64(42) {
		t.Fatalf("account arg = %v", args[0])
	}
	if args[1].(time.Time) != from || args[2].(time.Time) != to {
		t.Fatalf("window args = %v", args)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	r0 := rows[0]
	if r0.AccountID != 42 || r0.Symbol != "EUR/USD" || r0.InstrumentID != 9 {
		t.Fatalf("row = %+v", r0)
	}
	if !r0.Day.Equal(time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("day = %v", r0.Day)
	}
	// Net = realized + unrealized − fees = 10.5 − 2.25 − 0.75 = 7.5
	if got := r0.Net(); !got.Equal(decimal.RequireFromString("7.5")) {
		t.Fatalf("net = %s", got)
	}
	if r0.Ver != 100 {
		t.Fatalf("ver = %d", r0.Ver)
	}
}

func TestPnLReportErrors(t *testing.T) {
	if _, err := NewPnLStore(nil).Report(context.Background(), 1,
		pnlFixedNow, pnlFixedNow); err == nil {
		t.Fatal("nil conn must fail closed")
	}
	if _, err := NewPnLStore(&pnlFakeConn{}).Report(context.Background(), -1,
		pnlFixedNow, pnlFixedNow); err == nil {
		t.Fatal("negative account id must fail")
	}
	conn := &pnlFakeConn{queryErr: errSentinel{}}
	if _, err := NewPnLStore(conn).Report(context.Background(), 1,
		pnlFixedNow, pnlFixedNow.Add(24*time.Hour)); err == nil {
		t.Fatal("query error must propagate")
	}
}

// ---------------------------------------------------------------------------
// Gated live test — dev ClickHouse, EXC_CH_TEST=1. Skips while the
// sibling-owned DDL has not been applied (ShowCreateTable probe, same
// convention as ticks_test.go).
// ---------------------------------------------------------------------------

func TestCHPnLUpsertReport(t *testing.T) {
	conn := chTestConn(t)
	ctx := context.Background()
	ddl, err := ShowCreateTable(ctx, conn, TableAccountPnL)
	if err != nil {
		t.Skipf("account_pnl DDL not yet deployed (sibling schema pending): %v", err)
	}
	// Older dev deployments carry the interim variant (currency/
	// realized_pnl/fee_total, no instrument_id) — skip rather than fail
	// until the landed 005 DDL is applied. Backtick-quoted names avoid
	// substring false-positives (realized vs realized_pnl).
	for _, col := range []string{"`instrument_id`", "`realized`",
		"`unrealized`", "`fees`", "`ts`"} {
		if !strings.Contains(ddl, col) {
			t.Skipf("deployed account_pnl predates landed schema (missing %s):\n%s", col, ddl)
		}
	}
	s := NewPnLStore(conn)
	day := dayOnlyUTC(time.Now())
	row := PnLRow{AccountID: 999999001, InstrumentID: 9901, Symbol: "TEST/PAIR",
		Day: day, Realized: decimal.RequireFromString("1.5"),
		Unrealized: decimal.RequireFromString("0.5"),
		Fees:       decimal.RequireFromString("0.25")}
	if err := s.Upsert(ctx, row); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Second version supersedes the first (ReplacingMergeTree + FINAL).
	row.Realized = decimal.RequireFromString("2.5")
	time.Sleep(50 * time.Millisecond)
	if err := s.Upsert(ctx, row); err != nil {
		t.Fatalf("upsert v2: %v", err)
	}
	var got []PnLRow
	for i := 0; i < 20; i++ {
		var err error
		got, err = s.Report(ctx, row.AccountID, day, day.Add(24*time.Hour))
		if err != nil {
			t.Fatalf("report: %v", err)
		}
		if len(got) == 1 {
			break
		}
		time.Sleep(100 * time.Millisecond) // async_insert visibility lag
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1 (replace dedup): %+v", len(got), got)
	}
	if !got[0].Realized.Equal(decimal.RequireFromString("2.5")) {
		t.Fatalf("realized = %s, want 2.5", got[0].Realized)
	}
	if got[0].InstrumentID != 9901 {
		t.Fatalf("instrument_id = %d", got[0].InstrumentID)
	}
}
