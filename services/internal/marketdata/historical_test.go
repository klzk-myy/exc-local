// Phase-23 Task 23.3.1 — trades-store unit tests (fake CHQuerier; no
// live ClickHouse).
package marketdata

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"exchange/pkg/decimal"
)

// tradeFakeRows is a minimal driver.Rows stand-in over [][]any.
type tradeFakeRows struct {
	data [][]any
	idx  int
	err  error
}

func (r *tradeFakeRows) Next() bool { r.idx++; return r.idx <= len(r.data) }

func (r *tradeFakeRows) Scan(dest ...any) error {
	row := r.data[r.idx-1]
	if len(dest) != len(row) {
		return fmt.Errorf("scan arity %d != %d", len(dest), len(row))
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *int64:
			*d = row[i].(int64)
		case *uint64:
			*d = row[i].(uint64)
		case *uint32:
			*d = row[i].(uint32)
		case *string:
			*d = row[i].(string)
		case *time.Time:
			*d = row[i].(time.Time)
		case *decimal.Decimal:
			*d = row[i].(decimal.Decimal)
		default:
			return fmt.Errorf("assign %T <- %T", dest[i], row[i])
		}
	}
	return nil
}

func (r *tradeFakeRows) ScanStruct(any) error             { return fmt.Errorf("unimplemented") }
func (r *tradeFakeRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *tradeFakeRows) Totals(...any) error              { return nil }
func (r *tradeFakeRows) Columns() []string                { return nil }
func (r *tradeFakeRows) Close() error                     { return nil }
func (r *tradeFakeRows) Err() error                       { return r.err }

// tradeFakeConn captures the SQL + args the store issues and feeds
// canned rows back.
type tradeFakeConn struct {
	queries   []string
	queryArgs [][]any
	rows      [][]any
	err       error
}

func (c *tradeFakeConn) Query(_ context.Context, q string, args ...any) (driver.Rows, error) {
	c.queries = append(c.queries, q)
	c.queryArgs = append(c.queryArgs, args)
	if c.err != nil {
		return nil, c.err
	}
	return &tradeFakeRows{data: c.rows}, nil
}

func tradeRow(ts time.Time, id uint64) []any {
	return []any{ts, "EUR/USD", id, int64(7),
		int64(501), int64(502), uint64(9001), uint64(9002),
		decimal.MustFromString("1.0852"), decimal.MustFromString("1000"),
		"BUY", id * 10, uint32(3)}
}

func TestTradeHistoryStoreQuerySQL(t *testing.T) {
	conn := &tradeFakeConn{}
	st := NewTradeHistoryStore(conn)
	from := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	cur := &TradeHistoryCursor{Ts: to.Add(-time.Hour), TradeID: 42}
	_, err := st.Query(context.Background(), TradeHistoryQuery{
		Symbol: "EUR/USD", From: from, To: to, After: cur, Limit: 250,
	})
	if err != nil {
		t.Fatal(err)
	}
	q := conn.queries[0]
	for _, frag := range []string{
		"FROM trades FINAL",
		"WHERE symbol = ?",
		"AND ts >= ?",
		"AND ts < ?",
		"AND (ts, trade_id) < (?, ?)",
		"ORDER BY ts DESC, trade_id DESC",
		"LIMIT ?",
	} {
		if !strings.Contains(q, frag) {
			t.Fatalf("SQL missing %q: %s", frag, q)
		}
	}
	args := conn.queryArgs[0]
	if len(args) != 6 || args[0] != "EUR/USD" || args[5] != 250 {
		t.Fatalf("args = %v", args)
	}
	if !args[1].(time.Time).Equal(from) || !args[2].(time.Time).Equal(to) {
		t.Fatalf("bounds args = %v", args[1:3])
	}
	if !args[3].(time.Time).Equal(cur.Ts) || args[4] != cur.TradeID {
		t.Fatalf("cursor args = %v", args[3:5])
	}
}

func TestTradeHistoryStoreDefaultsAndScan(t *testing.T) {
	ts := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	conn := &tradeFakeConn{rows: [][]any{tradeRow(ts, 1002)}}
	st := NewTradeHistoryStore(conn)
	out, err := st.Query(context.Background(), TradeHistoryQuery{Symbol: "EUR/USD"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("rows = %d", len(out))
	}
	tr := out[0]
	if tr.TradeID != 1002 || tr.Symbol != "EUR/USD" || tr.EventSeq != 10020 ||
		tr.ShardID != 3 || tr.AggressorSide != "BUY" || tr.InstrumentID != 7 {
		t.Fatalf("row = %+v", tr)
	}
	if tr.Participants.MakerAccountID != 501 || tr.Participants.TakerAccountID != 502 ||
		tr.Participants.BuyOrderID != 9001 || tr.Participants.SellOrderID != 9002 {
		t.Fatalf("participants = %+v", tr.Participants)
	}
	if !tr.Price.Equal(decimal.MustFromString("1.0852")) ||
		!tr.Qty.Equal(decimal.MustFromString("1000")) {
		t.Fatalf("price/qty = %s/%s", tr.Price, tr.Qty)
	}
	// Limit 0 → store default 1000 (last arg).
	args := conn.queryArgs[0]
	if args[len(args)-1] != 1000 {
		t.Fatalf("default limit arg = %v", args[len(args)-1])
	}
	// Unbounded query: only symbol + LIMIT args.
	if len(args) != 2 {
		t.Fatalf("unbounded args = %v", args)
	}
}

func TestTradeHistoryStoreFailClosed(t *testing.T) {
	conn := &tradeFakeConn{err: errors.New("ch down")}
	st := NewTradeHistoryStore(conn)
	out, err := st.Query(context.Background(), TradeHistoryQuery{Symbol: "EUR/USD"})
	if err == nil || out != nil {
		t.Fatalf("outage must surface an error, got %v / %v", out, err)
	}
	// Scan mismatch propagates.
	conn.err = nil
	conn.rows = [][]any{{"short"}}
	if _, err := st.Query(context.Background(), TradeHistoryQuery{Symbol: "EUR/USD"}); err == nil {
		t.Fatal("short row must scan-fail")
	}
}
