// Phase-23 Task 23.3.7 — block-tape store unit tests (fake CHTapeConn;
// no live ClickHouse). Covers the SQL contract, the anonymous scan
// projection, correction lineage resolution, and the sink write path.
package marketdata

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"exchange/pkg/decimal"
)

// tapeFakeRows is a minimal driver.Rows stand-in over [][]any — same
// pattern as historical_test.go's tradeFakeRows, extended for the
// tape projection ([]string venue flags, **decimal.Decimal NULLs).
type tapeFakeRows struct {
	data [][]any
	idx  int
	err  error
}

func (r *tapeFakeRows) Next() bool { r.idx++; return r.idx <= len(r.data) }

func (r *tapeFakeRows) Scan(dest ...any) error {
	row := r.data[r.idx-1]
	if len(dest) != len(row) {
		return fmt.Errorf("scan arity %d != %d", len(dest), len(row))
	}
	for i := range dest {
		if row[i] == nil {
			continue
		}
		switch d := dest[i].(type) {
		case *int64:
			*d = row[i].(int64)
		case *uint64:
			*d = row[i].(uint64)
		case *string:
			*d = row[i].(string)
		case *[]string:
			*d = row[i].([]string)
		case *time.Time:
			*d = row[i].(time.Time)
		case *decimal.Decimal:
			*d = row[i].(decimal.Decimal)
		case **decimal.Decimal:
			*d = row[i].(*decimal.Decimal)
		default:
			return fmt.Errorf("assign %T <- %T", dest[i], row[i])
		}
	}
	return nil
}

func (r *tapeFakeRows) ScanStruct(any) error             { return fmt.Errorf("unimplemented") }
func (r *tapeFakeRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *tapeFakeRows) Totals(...any) error              { return nil }
func (r *tapeFakeRows) Columns() []string                { return nil }
func (r *tapeFakeRows) Close() error                     { return nil }
func (r *tapeFakeRows) Err() error                       { return r.err }

// tapeFakeBatch captures appended rows; Send/Abort record the outcome.
type tapeFakeBatch struct {
	appended [][]any
	sent     bool
	aborted  bool
	err      error
}

func (b *tapeFakeBatch) Abort() error { b.aborted = true; return nil }
func (b *tapeFakeBatch) Append(v ...any) error {
	b.appended = append(b.appended, v)
	return b.err
}
func (b *tapeFakeBatch) AppendStruct(any) error        { return fmt.Errorf("unimplemented") }
func (b *tapeFakeBatch) Column(int) driver.BatchColumn { return nil }
func (b *tapeFakeBatch) Flush() error                  { return nil }
func (b *tapeFakeBatch) Send() error                   { b.sent = true; return nil }
func (b *tapeFakeBatch) IsSent() bool                  { return b.sent }
func (b *tapeFakeBatch) Rows() int                     { return len(b.appended) }
func (b *tapeFakeBatch) Columns() []column.Interface   { return nil }
func (b *tapeFakeBatch) Close() error                  { return nil }

// tapeFakeConn dispatches canned rows per query (the main page read vs
// the correction-lineage lookup) and captures batches.
type tapeFakeConn struct {
	queries   []string
	queryArgs [][]any
	answer    func(q string, args []any) ([][]any, error)
	batches   []string
	batch     *tapeFakeBatch
	err       error
}

func (c *tapeFakeConn) Query(_ context.Context, q string, args ...any) (driver.Rows, error) {
	c.queries = append(c.queries, q)
	c.queryArgs = append(c.queryArgs, args)
	if c.err != nil {
		return nil, c.err
	}
	rows, err := c.answer(q, args)
	if err != nil {
		return nil, err
	}
	return &tapeFakeRows{data: rows}, nil
}

func (c *tapeFakeConn) PrepareBatch(_ context.Context, q string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	c.batches = append(c.batches, q)
	if c.batch == nil {
		c.batch = &tapeFakeBatch{}
	}
	return c.batch, nil
}

// tapePrintRow assembles one read-projection print row.
func tapePrintRow(id uint64, exec, pub time.Time) []any {
	return []any{
		blockTapeEntryID(id, false), "PRINT", id, "EUR/USD",
		decimal.MustFromString("1.08520125"), decimal.MustFromString("920000"),
		decimal.MustFromString("998231.15"), exec, pub, int64(900000),
		[]string{"DELAYED"}, uint64(0), (*decimal.Decimal)(nil), (*decimal.Decimal)(nil),
	}
}

// tapeCorrRow assembles one read-projection correction row.
func tapeCorrRow(id uint64, kind string, corr time.Time, orig uint64) []any {
	cp := decimal.MustFromString("1.0853")
	cq := decimal.MustFromString("910000")
	return []any{
		blockTapeEntryID(id, true), kind, id, "EUR/USD",
		decimal.Zero, decimal.Zero, decimal.Zero, corr, corr, int64(0),
		[]string{}, orig, &cp, &cq,
	}
}

func TestBlockTapeStoreQuerySQL(t *testing.T) {
	conn := &tapeFakeConn{answer: func(string, []any) ([][]any, error) { return nil, nil }}
	st := NewBlockTapeStore(conn)
	from := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	pub := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	cur := &BlockTapeCursor{Ts: to.Add(-time.Hour), EntryID: 42}
	_, err := st.Query(context.Background(), BlockTapeQuery{
		Symbol: "EUR/USD", From: from, To: to,
		PublishedBefore: pub, After: cur, Limit: 250,
	})
	if err != nil {
		t.Fatal(err)
	}
	q := conn.queries[0]
	for _, frag := range []string{
		"FROM block_trades_tape FINAL",
		"WHERE symbol = ?",
		"AND exec_ts >= ?",
		"AND exec_ts < ?",
		"AND pub_ts <= ?",
		"AND (pub_ts, entry_id) < (?, ?)",
		"ORDER BY pub_ts DESC, entry_id DESC",
		"LIMIT ?",
	} {
		if !strings.Contains(q, frag) {
			t.Fatalf("SQL missing %q: %s", frag, q)
		}
	}
	args := conn.queryArgs[0]
	if len(args) != 7 || args[0] != "EUR/USD" || args[6] != 250 {
		t.Fatalf("args = %v", args)
	}
	if !args[3].(time.Time).Equal(pub) {
		t.Fatalf("PublishedBefore arg = %v", args[3])
	}
	if !args[4].(time.Time).Equal(cur.Ts) || args[5] != cur.EntryID {
		t.Fatalf("cursor args = %v", args[4:6])
	}
}

func TestBlockTapeStoreLineageAnnotation(t *testing.T) {
	exec := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	pub := exec.Add(15 * time.Minute)
	corr := pub.Add(20 * time.Minute)
	conn := &tapeFakeConn{answer: func(q string, _ []any) ([][]any, error) {
		if strings.Contains(q, "kind != 'PRINT'") {
			// Correction-lineage lookup: print 1002 busted by entry 2005.
			return [][]any{{uint64(1002), "BUST", blockTapeEntryID(1002, true)}}, nil
		}
		return [][]any{
			tapePrintRow(1002, exec, pub),
			tapeCorrRow(1002, "BUST", corr, 77),
		}, nil
	}}
	st := NewBlockTapeStore(conn)
	out, err := st.Query(context.Background(), BlockTapeQuery{Symbol: "EUR/USD"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("rows = %d", len(out))
	}
	p, c := out[0], out[1]
	if p.Kind != BlockTapePrint || !p.Bust || p.CorrectedBy != blockTapeEntryID(1002, true) {
		t.Fatalf("print lineage = %+v", p)
	}
	if p.Price.String() != "1.08520125" || p.DelayMs != 900000 {
		t.Fatalf("print = %+v", p)
	}
	if !c.IsCorrection() || c.Kind != BlockTapeBust || c.BlockTradeID != 1002 ||
		c.OriginalTradeID != 77 {
		t.Fatalf("correction = %+v", c)
	}
	if c.CorrectedPrice == nil || c.CorrectedPrice.String() != "1.0853" {
		t.Fatalf("corrected price = %v", c.CorrectedPrice)
	}
	if !c.ExecTs.Equal(corr) {
		t.Fatalf("correction exec_ts must carry the correction event time, got %v", c.ExecTs)
	}
}

func TestBlockTapeStoreRecord(t *testing.T) {
	conn := &tapeFakeConn{}
	st := NewBlockTapeStore(conn)
	exec := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	pub := exec.Add(15 * time.Minute)
	if err := st.Record(context.Background(), blockTradeData{
		Event: "blockTrade", BlockTradeID: 1002, Symbol: "EUR/USD",
		Price: "1.08520125", Quantity: "920000", NotionalUSD: "998231.15",
		ExecTsMs: exec.UnixMilli(), PubTsMs: pub.UnixMilli(),
		DelayMs: 900000, VenueFlags: []string{"DELAYED"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(conn.batches) != 1 || !strings.Contains(conn.batches[0], "block_trades_tape") {
		t.Fatalf("batches = %v", conn.batches)
	}
	row := conn.batch.appended[0]
	if row[0] != blockTapeEntryID(1002, false) || row[0].(uint64)&1 != 0 {
		t.Fatalf("print entry_id = %v", row[0])
	}
	if row[1] != "PRINT" || row[2] != uint64(1002) || row[3] != "EUR/USD" {
		t.Fatalf("row = %v", row[:4])
	}
	if !row[7].(time.Time).Equal(exec) || !row[8].(time.Time).Equal(pub) {
		t.Fatalf("ts = %v / %v", row[7], row[8])
	}
	if row[11] != uint64(0) || row[12] != nil || row[13] != nil {
		t.Fatalf("print must carry no correction payload: %v", row[11:14])
	}
	if !conn.batch.sent {
		t.Fatal("batch never sent")
	}
}

func TestBlockTapeStoreRecordCorrection(t *testing.T) {
	conn := &tapeFakeConn{}
	st := NewBlockTapeStore(conn)
	corr := time.Date(2026, 10, 5, 14, 45, 0, 0, time.UTC)
	if err := st.RecordCorrection(context.Background(), blockCorrectionData{
		Event: "blockTradeCorrection", Kind: "BUST",
		BlockTradeID: 1002, OriginalTradeID: 77, Symbol: "EUR/USD",
		TsMs: corr.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	row := conn.batch.appended[0]
	if row[0] != blockTapeEntryID(1002, true) || row[0].(uint64)&1 != 1 {
		t.Fatalf("correction entry_id = %v", row[0])
	}
	if row[1] != "BUST" || row[2] != uint64(1002) || row[3] != "EUR/USD" {
		t.Fatalf("row = %v", row[:4])
	}
	// exec_ts = correction event time (interleaves under exec bounds).
	if !row[7].(time.Time).Equal(corr) || !row[8].(time.Time).Equal(corr) {
		t.Fatalf("ts = %v / %v", row[7], row[8])
	}
	if row[11] != uint64(77) {
		t.Fatalf("original_trade_id = %v", row[11])
	}
	if !conn.batch.sent {
		t.Fatal("batch never sent")
	}
}

func TestBlockTapeStoreFailClosed(t *testing.T) {
	conn := &tapeFakeConn{err: errors.New("ch down")}
	st := NewBlockTapeStore(conn)
	out, err := st.Query(context.Background(), BlockTapeQuery{Symbol: "EUR/USD"})
	if err == nil || out != nil {
		t.Fatalf("outage must surface an error, got %v / %v", out, err)
	}
}

// TestBlockTapeProjectionAnonymous pins the anonymity invariant: the
// stored/selected column tuple carries no participant identifier
// columns — the wire struct has none to mask.
func TestBlockTapeProjectionAnonymous(t *testing.T) {
	banned := []string{"account", "order_id", "side", "participant", "counterparty"}
	for _, cols := range []string{blockTapeColumns, blockTapeReadColumns} {
		lc := strings.ToLower(cols)
		for _, b := range banned {
			if strings.Contains(lc, b) {
				t.Fatalf("tape columns expose %q: %s", b, cols)
			}
		}
	}
}

func TestWriteBlockTapeCSV(t *testing.T) {
	exec := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	pub := exec.Add(15 * time.Minute)
	corr := pub.Add(20 * time.Minute)
	cp := decimal.MustFromString("1.0853")
	entries := []*BlockTapeEntry{
		{EntryID: blockTapeEntryID(1002, false), Kind: BlockTapePrint,
			BlockTradeID: 1002, Symbol: "EUR/USD",
			Price: decimal.MustFromString("1.08520125"), Quantity: decimal.MustFromString("920000"),
			NotionalUSD: decimal.MustFromString("998231.15"),
			ExecTs:      exec, PubTs: pub, DelayMs: 900000,
			VenueFlags: []string{"DELAYED"}, CorrectedBy: blockTapeEntryID(1002, true), Bust: true},
		{EntryID: blockTapeEntryID(1002, true), Kind: BlockTapeBust,
			BlockTradeID: 1002, Symbol: "EUR/USD",
			ExecTs: corr, PubTs: corr, OriginalTradeID: 77,
			CorrectedPrice: &cp},
	}
	var buf strings.Builder
	if err := WriteBlockTapeCSV(&buf, entries); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d", len(lines))
	}
	if !strings.HasPrefix(lines[0], "entry_id,kind,block_trade_id") ||
		!strings.Contains(lines[0], "corrected_by,supersedes") {
		t.Fatalf("header = %s", lines[0])
	}
	if !strings.Contains(lines[1], ",PRINT,") || !strings.Contains(lines[1], "1.08520125") ||
		!strings.Contains(lines[1], "true") {
		t.Fatalf("print row = %s", lines[1])
	}
	if !strings.Contains(lines[2], ",BUST,") || !strings.Contains(lines[2], ",1002,") ||
		!strings.Contains(lines[2], "1.0853") {
		t.Fatalf("correction row = %s", lines[2])
	}
}
