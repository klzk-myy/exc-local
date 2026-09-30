// Phase-23 Task 23.3.10 tests — taker-volume/positioning reads over the
// ClickHouse trades projection (§10.8, §24 #359). The CH seam is a
// queued fake; queries are asserted, rows are canned.
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

// flowFakeRows / flowFakeConn — the same driver.Rows stand-in pattern
// as historical_test.go, but the store issues TWO queries per read
// (sums + cohort), so responses are a per-call queue.
type flowFakeRows struct {
	data [][]any
	idx  int
	err  error
}

func (r *flowFakeRows) Next() bool { r.idx++; return r.idx <= len(r.data) }

func (r *flowFakeRows) Scan(dest ...any) error {
	row := r.data[r.idx-1]
	if len(dest) != len(row) {
		return fmt.Errorf("scan arity %d != %d", len(dest), len(row))
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *decimal.Decimal:
			*d = row[i].(decimal.Decimal)
		case *uint64:
			*d = row[i].(uint64)
		case *time.Time:
			*d = row[i].(time.Time)
		default:
			return fmt.Errorf("assign %T <- %T", dest[i], row[i])
		}
	}
	return nil
}

func (r *flowFakeRows) ScanStruct(any) error             { return fmt.Errorf("unimplemented") }
func (r *flowFakeRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *flowFakeRows) Totals(...any) error              { return nil }
func (r *flowFakeRows) Columns() []string                { return nil }
func (r *flowFakeRows) Close() error                     { return nil }
func (r *flowFakeRows) Err() error                       { return r.err }

// flowFakeConn serves queued responses in call order.
type flowFakeConn struct {
	queries   []string
	queryArgs [][]any
	queue     []*flowFakeRows
	err       error
}

func (c *flowFakeConn) Query(_ context.Context, q string, args ...any) (driver.Rows, error) {
	c.queries = append(c.queries, q)
	c.queryArgs = append(c.queryArgs, args)
	if c.err != nil {
		return nil, c.err
	}
	if len(c.queue) == 0 {
		return &flowFakeRows{}, nil
	}
	r := c.queue[0]
	c.queue = c.queue[1:]
	return r, nil
}

func posDec(s string) decimal.Decimal { return decimal.MustFromString(s) }

// flowSumRow mirrors the takerSumCols projection: buy/sell/unknown
// notional, buy/sell volume, trade count.
func flowSumRow(buyN, sellN, unkN, buyV, sellV string, trades uint64) []any {
	return []any{posDec(buyN), posDec(sellN), posDec(unkN), posDec(buyV), posDec(sellV), trades}
}

func TestTakerFlowWindow(t *testing.T) {
	from := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	to := from.Add(5 * time.Minute)
	conn := &flowFakeConn{queue: []*flowFakeRows{
		{data: [][]any{flowSumRow("600000", "300000", "10000",
			"550", "260", 1200)}},
		{data: [][]any{{uint64(250)}}}, // distinct maker∪taker cohort
	}}
	st := NewTakerFlowStore(conn)
	f, err := st.TakerFlow(context.Background(), "EUR/USD", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(conn.queries) != 2 {
		t.Fatalf("queries=%d want 2 (sums + cohort)", len(conn.queries))
	}
	if !strings.Contains(conn.queries[0], "aggressor_side") ||
		!strings.Contains(conn.queries[0], "FROM trades FINAL") {
		t.Fatalf("sums query off-shape: %s", conn.queries[0])
	}
	if !strings.Contains(conn.queries[1], "uniqExact") ||
		!strings.Contains(conn.queries[1], "maker_account_id") ||
		!strings.Contains(conn.queries[1], "taker_account_id") {
		t.Fatalf("cohort query must union maker+taker: %s", conn.queries[1])
	}
	if f.Accounts != 250 || f.Trades != 1200 {
		t.Fatalf("flow=%+v", f)
	}
	if f.BuySellRatio() == nil || !f.BuySellRatio().Equal(posDec("2")) {
		t.Fatalf("ratio=%v", f.BuySellRatio())
	}
	// Reconciliation: buy+sell+unknown notional = full fill notional.
	if !f.Total().Equal(posDec("910000")) {
		t.Fatalf("total=%s want 910000", f.Total())
	}
}

func TestTakerFlowWindowZeroSell(t *testing.T) {
	conn := &flowFakeConn{queue: []*flowFakeRows{
		{data: [][]any{flowSumRow("100", "0", "0", "10", "0", 5)}},
		{data: [][]any{{uint64(0)}}},
	}}
	st := NewTakerFlowStore(conn)
	f, err := st.TakerFlow(context.Background(), "EUR/USD",
		time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if f.BuySellRatio() != nil {
		t.Fatal("zero sell side must produce a nil ratio, not ∞/0")
	}
}

func TestTakerFlowBuckets(t *testing.T) {
	b0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	b1 := b0.Add(5 * time.Minute)
	conn := &flowFakeConn{queue: []*flowFakeRows{
		{data: [][]any{
			append([]any{b0}, flowSumRow("600", "300", "0", "60", "30", 100)...),
			append([]any{b1}, flowSumRow("900", "100", "0", "90", "10", 200)...),
		}},
		{data: [][]any{
			{b0, uint64(120)}, // b0 above the 100-account floor
			{b1, uint64(30)},  // b1 below — per-bucket suppression basis
		}},
	}}
	st := NewTakerFlowStore(conn)
	buckets, err := st.Buckets(context.Background(), "EUR/USD",
		b0, b1.Add(5*time.Minute), 300)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 {
		t.Fatalf("buckets=%d", len(buckets))
	}
	if buckets[0].Flow.Accounts != 120 || buckets[1].Flow.Accounts != 30 {
		t.Fatalf("per-bucket cohorts not merged: %+v", buckets)
	}
	if !buckets[0].Flow.BuySellRatio().Equal(posDec("2")) {
		t.Fatalf("b0 ratio=%v", buckets[0].Flow.BuySellRatio())
	}
	if strings.Contains(conn.queries[1], "GROUP BY bucket") == false {
		t.Fatalf("cohort query must bucket: %s", conn.queries[1])
	}
}

func TestTakerFlowStoreNotWired(t *testing.T) {
	st := NewTakerFlowStore(nil)
	if _, err := st.TakerFlow(context.Background(), "EUR/USD",
		time.Now(), time.Now()); !errors.Is(err, ErrNoTakerFlowStore) {
		t.Fatalf("err=%v want ErrNoTakerFlowStore", err)
	}
	if _, err := st.Buckets(context.Background(), "EUR/USD",
		time.Now(), time.Now(), 300); !errors.Is(err, ErrNoTakerFlowStore) {
		t.Fatalf("err=%v want ErrNoTakerFlowStore", err)
	}
}

func TestTakerFlowQueryError(t *testing.T) {
	conn := &flowFakeConn{err: errors.New("conn refused")}
	st := NewTakerFlowStore(conn)
	if _, err := st.TakerFlow(context.Background(), "EUR/USD",
		time.Now(), time.Now()); err == nil {
		t.Fatal("conn error must propagate")
	}
}
