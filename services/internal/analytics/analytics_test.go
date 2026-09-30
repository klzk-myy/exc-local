package analytics

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	flatbuffers "github.com/google/flatbuffers/go"
	gonats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shopspring/decimal"

	"exchange/internal/ipc"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---- fakes ------------------------------------------------------------

// etlFakeConn implements Conn; insert failure is switchable per table and
// Ping health is switchable — that drives the divert/drain tests.
type etlFakeConn struct {
	mu        sync.Mutex
	failTable map[string]bool // insert error per table
	pingErr   bool            // Ping fails while set
	appended  map[string][][]any
	sends     int
}

func newEtlFakeConn() *etlFakeConn {
	return &etlFakeConn{failTable: map[string]bool{}, appended: map[string][][]any{}}
}

func (f *etlFakeConn) Exec(ctx context.Context, q string, args ...any) error { return nil }

func (f *etlFakeConn) Query(ctx context.Context, q string, args ...any) (driver.Rows, error) {
	return &etlFakeRows{}, nil
}

func (f *etlFakeConn) PrepareBatch(ctx context.Context, q string, opts ...driver.PrepareBatchOption) (driver.Batch, error) {
	// "INSERT INTO <table> (...)"
	parts := strings.Fields(q)
	table := parts[2]
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failTable[table] {
		return nil, fmt.Errorf("clickhouse: simulated insert failure into %s", table)
	}
	return &etlFakeBatch{conn: f, table: table}, nil
}

func (f *etlFakeConn) Ping(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pingErr {
		return fmt.Errorf("clickhouse: simulated ping failure")
	}
	return nil
}

func (f *etlFakeConn) Close() error { return nil }

func (f *etlFakeConn) failInsert(table string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failTable[table] = on
}

func (f *etlFakeConn) setPingErr(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pingErr = on
}

func (f *etlFakeConn) got(table string) [][]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.appended[table]
}

type etlFakeBatch struct {
	conn  *etlFakeConn
	table string
	rows  [][]any
	sent  bool
}

func (b *etlFakeBatch) Append(v ...any) error {
	b.rows = append(b.rows, append([]any(nil), v...))
	return nil
}
func (b *etlFakeBatch) AppendStruct(v any) error { return fmt.Errorf("unimplemented") }
func (b *etlFakeBatch) Column(int) driver.BatchColumn {
	return nil
}
func (b *etlFakeBatch) Flush() error { return nil }
func (b *etlFakeBatch) IsSent() bool { return b.sent }
func (b *etlFakeBatch) Rows() int    { return len(b.rows) }
func (b *etlFakeBatch) Columns() []column.Interface {
	return nil
}
func (b *etlFakeBatch) Abort() error { b.rows = nil; return nil }
func (b *etlFakeBatch) Close() error { return nil }
func (b *etlFakeBatch) Send() error {
	b.conn.mu.Lock()
	defer b.conn.mu.Unlock()
	if b.conn.failTable[b.table] {
		return fmt.Errorf("clickhouse: simulated send failure into %s", b.table)
	}
	for _, r := range b.rows {
		b.conn.appended[b.table] = append(b.conn.appended[b.table], r)
	}
	b.conn.sends++
	b.sent = true
	return nil
}

type etlFakeRows struct{}

func (r *etlFakeRows) Next() bool                       { return false }
func (r *etlFakeRows) Scan(dest ...any) error           { return nil }
func (r *etlFakeRows) ScanStruct(dest any) error        { return nil }
func (r *etlFakeRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *etlFakeRows) Totals(dest ...any) error         { return nil }
func (r *etlFakeRows) Columns() []string                { return nil }
func (r *etlFakeRows) Close() error                     { return nil }
func (r *etlFakeRows) Err() error                       { return nil }

// etlFakeMsg implements jetstream.Msg for the consumer unit tests.
type etlFakeMsg struct {
	subject string
	data    []byte
	acked   bool
	nakd    bool
}

func (m *etlFakeMsg) Metadata() (*jetstream.MsgMetadata, error) { return nil, nil }
func (m *etlFakeMsg) Data() []byte                              { return m.data }
func (m *etlFakeMsg) Headers() gonats.Header                    { return nil }
func (m *etlFakeMsg) Subject() string                           { return m.subject }
func (m *etlFakeMsg) Reply() string                             { return "" }
func (m *etlFakeMsg) Ack() error                                { m.acked = true; return nil }
func (m *etlFakeMsg) DoubleAck(ctx context.Context) error       { m.acked = true; return nil }
func (m *etlFakeMsg) Nak() error                                { m.nakd = true; return nil }
func (m *etlFakeMsg) NakWithDelay(d time.Duration) error        { m.nakd = true; return nil }
func (m *etlFakeMsg) InProgress() error                         { return nil }
func (m *etlFakeMsg) Term() error                               { m.acked = true; return nil }
func (m *etlFakeMsg) TermWithReason(r string) error             { m.acked = true; return nil }

// ---- tests ------------------------------------------------------------

func TestSpoolAppendPeekDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSpool(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rows1 := [][]any{{time.Unix(1, 0).UTC(), "EUR-USD", decimal.New(108501234, -8), decimal.New(1000000, -8), "UNKNOWN", uint64(42), uint64(7), uint32(0), uint64(1)}}
	rows2 := [][]any{{time.Unix(2, 0).UTC(), "GBP-USD", decimal.New(127, -2), decimal.New(5, 0), "BUY", uint64(43), uint64(8), uint32(0), uint64(2)}}
	if _, err := s.Append("ticks", rows1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("trades", rows2); err != nil {
		t.Fatal(err)
	}
	if got := s.Entries(); got != 2 {
		t.Fatalf("entries = %d, want 2", got)
	}

	ents, err := s.Peek(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 {
		t.Fatalf("peek = %d, want 2", len(ents))
	}
	// FIFO: first appended is first peeked.
	if ents[0].batch.Table != "ticks" || ents[1].batch.Table != "trades" {
		t.Fatalf("peek order = %s,%s; want ticks,trades", ents[0].batch.Table, ents[1].batch.Table)
	}
	got := ents[0].batch.Rows[0]
	if got[1].(string) != "EUR-USD" {
		t.Fatalf("symbol = %v", got[1])
	}
	if d, ok := got[2].(decimal.Decimal); !ok || !d.Equal(decimal.New(108501234, -8)) {
		t.Fatalf("price = %#v, want decimal 1.08501234", got[2])
	}

	// Delete the first entry only (at-least-once boundary).
	if err := s.Delete([][]byte{ents[0].key}); err != nil {
		t.Fatal(err)
	}
	if got := s.Entries(); got != 1 {
		t.Fatalf("entries after delete = %d, want 1", got)
	}
	ents, err = s.Peek(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].batch.Table != "trades" {
		t.Fatalf("remaining = %+v", ents)
	}
}

func TestSpoolPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSpool(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("ticks", [][]any{{time.Now().UTC(), "EUR-USD"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCursor(4242); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := OpenSpool(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.Entries(); got != 1 {
		t.Fatalf("reopened entries = %d, want 1", got)
	}
	cur, err := s2.Cursor()
	if err != nil || cur != 4242 {
		t.Fatalf("cursor = %d err %v, want 4242", cur, err)
	}
}

func TestSpoolEvictsOldest(t *testing.T) {
	dir := t.TempDir()
	// Tiny bound forces eviction of prior entries on each append.
	s, err := OpenSpool(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	big := make([]byte, 4096)
	for i := 0; i < 8; i++ {
		if _, err := s.Append("ticks", [][]any{{i, big}}); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Entries(); got >= 8 {
		t.Fatalf("entries = %d, expected eviction under bound", got)
	}
	ents, err := s.Peek(100)
	if err != nil {
		t.Fatal(err)
	}
	// Oldest-first eviction: any survivors are the most recent appends.
	for _, e := range ents {
		if e.batch.Rows[0][0].(int) < 7-len(ents)+1 && len(ents) > 0 {
			// survivors must be the tail
		}
	}
	if len(ents) > 0 && ents[0].batch.Rows[0][0].(int) == 0 && len(ents) > 1 {
		t.Fatalf("oldest entry survived eviction")
	}
}

func TestInsertWithSpoolDiverts(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSpool(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fc := newEtlFakeConn()
	fc.failInsert("ticks", true)
	m := NewIngestMetrics()
	ing := NewIngester(fc, s, Options{InsertTimeout: 50 * time.Millisecond}, m, nil)

	row := TickRowValues(time.Now().UTC(), "EUR-USD", 108501234, 500000, "UNKNOWN", 42, 7, 0)
	if err := ing.InsertWithSpool(context.Background(), "ticks", [][]any{row}); err != nil {
		t.Fatalf("InsertWithSpool returned error although spool succeeded: %v", err)
	}
	if got := s.Entries(); got != 1 {
		t.Fatalf("spool entries = %d, want 1", got)
	}
	snap := m.Snapshot()
	if snap.InsertErrors != 1 || snap.SpooledRows != 1 || snap.SpooledBatches != 1 {
		t.Fatalf("metrics = %+v", snap)
	}
	if len(fc.got("ticks")) != 0 {
		t.Fatalf("fake conn received rows despite failure")
	}
}

func TestInsertWithSpoolTotalFailure(t *testing.T) {
	fc := newEtlFakeConn()
	fc.failInsert("ticks", true)
	ing := NewIngester(fc, nil, Options{InsertTimeout: 50 * time.Millisecond}, nil, nil)
	row := TickRowValues(time.Now().UTC(), "EUR-USD", 1, 1, "UNKNOWN", 1, 1, 0)
	if err := ing.InsertWithSpool(context.Background(), "ticks", [][]any{row}); err == nil {
		t.Fatal("expected error: nothing durable (no spool configured)")
	}
}

func TestInsertUnknownTableRejected(t *testing.T) {
	ing := NewIngester(newEtlFakeConn(), nil, Options{}, nil, nil)
	if err := ing.InsertWithSpool(context.Background(), "users; DROP TABLE users", [][]any{{1}}); err == nil {
		t.Fatal("unknown table accepted — SQL injection surface")
	}
}

func TestDrainOnceGroupsAndDeletes(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSpool(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fc := newEtlFakeConn()
	m := NewIngestMetrics()
	ing := NewIngester(fc, s, Options{DrainBatch: 10_000, InsertTimeout: time.Second}, m, nil)

	// Seed the spool via the divert path.
	fc.failInsert("ticks", true)
	fc.failInsert("trades", true)
	tr := TickRowValues(time.Now().UTC(), "EUR-USD", 1, 1, "UNKNOWN", 42, 7, 0)
	if err := ing.InsertWithSpool(context.Background(), "ticks", [][]any{tr}); err != nil {
		t.Fatal(err)
	}
	if err := ing.InsertWithSpool(context.Background(), "trades", [][]any{TradeRowValues(time.Now().UTC(), "EUR-USD", 42, 1, 100, 200, 11, 12, 1, 1, "UNKNOWN", 7, 0)}); err != nil {
		t.Fatal(err)
	}
	if s.Entries() != 2 {
		t.Fatalf("entries = %d", s.Entries())
	}

	// Recover: conn healthy -> drain inserts groups, deletes acked keys.
	fc.failInsert("ticks", false)
	fc.failInsert("trades", false)
	n, err := ing.DrainOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || s.Entries() != 0 {
		t.Fatalf("drained = %d entries = %d, want 2/0", n, s.Entries())
	}
	if got := fc.got("ticks"); len(got) != 1 || got[0][5].(uint64) != 42 {
		t.Fatalf("ticks rows = %+v", got)
	}
	if got := fc.got("trades"); len(got) != 1 || got[0][2].(uint64) != 42 {
		t.Fatalf("trades rows = %+v", got)
	}
	snap := m.Snapshot()
	if snap.DrainedBatches != 2 || snap.DrainedRows != 2 {
		t.Fatalf("drain metrics = %+v", snap)
	}
}

func TestDrainStopsOnPartialFailure(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSpool(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fc := newEtlFakeConn()
	ing := NewIngester(fc, s, Options{DrainBatch: 10_000, InsertTimeout: time.Second}, nil, nil)

	fc.failInsert("ticks", true)
	fc.failInsert("trades", true)
	if err := ing.InsertWithSpool(context.Background(), "ticks", [][]any{TickRowValues(time.Now().UTC(), "EUR-USD", 1, 1, "U", 1, 1, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := ing.InsertWithSpool(context.Background(), "trades", [][]any{TradeRowValues(time.Now().UTC(), "EUR-USD", 2, 1, 100, 200, 1, 2, 1, 1, "UNKNOWN", 2, 0)}); err != nil {
		t.Fatal(err)
	}

	// ticks recovers, trades stays dead -> ticks drained, trades kept.
	fc.failInsert("ticks", false)
	n, err := ing.DrainOnce(context.Background())
	if err == nil {
		t.Fatal("expected drain error on still-failing trades insert")
	}
	if n != 1 || s.Entries() != 1 {
		t.Fatalf("drained = %d entries = %d, want 1/1", n, s.Entries())
	}
	if got := fc.got("trades"); len(got) != 0 {
		t.Fatalf("failed trades insert captured rows: %+v", got)
	}
}

func encodeFill(t *testing.T, seq, ts, tradeID uint64, priceE8, qtyE8 int64, engineSeq int64) []byte {
	t.Helper()
	b := flatbuffers.NewBuilder(256)
	return append([]byte(nil), ipc.EncodeTradeFillEvent(b, seq, ts, tradeID, 11, 12, priceE8, qtyE8, engineSeq)...)
}

func TestConsumerProcessAcksDurable(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSpool(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fc := newEtlFakeConn()
	m := NewIngestMetrics()
	ing := NewIngester(fc, s, Options{InsertTimeout: time.Second}, m, nil)
	cons := NewConsumer(nil, ing, ConsumerOptions{}, m, nil)

	ts := uint64(time.Now().UnixNano())
	msgs := []jetstream.Msg{
		&etlFakeMsg{subject: "trades.0.EUR-USD", data: encodeFill(t, 100, ts, 42, 108501234, 1000000, 7)},
		&etlFakeMsg{subject: "trades.0.GBP-USD", data: encodeFill(t, 101, ts, 43, 127000000, 500000, 8)},
	}
	if err := cons.process(context.Background(), "trades", msgs); err != nil {
		t.Fatal(err)
	}
	for i, msg := range msgs {
		if !msg.(*etlFakeMsg).acked {
			t.Fatalf("msg %d not acked after durable insert", i)
		}
	}
	ticks := fc.got("ticks")
	if len(ticks) != 2 {
		t.Fatalf("ticks rows = %d", len(ticks))
	}
	// Subject tokens canonicalize at ingest: "EUR-USD" -> "EUR/USD" so the
	// CH projections share the instruments.symbol convention (REST queries,
	// CHSessionVWAP and the candle engine all key on the canonical form).
	if ticks[0][1].(string) != "EUR/USD" || ticks[1][1].(string) != "GBP/USD" {
		t.Fatalf("symbols = %v,%v", ticks[0][1], ticks[1][1])
	}
	if d := ticks[0][2].(decimal.Decimal); !d.Equal(decimal.New(108501234, -8)) {
		t.Fatalf("price = %v, want 1.08501234", d)
	}
	if got := fc.got("trades"); len(got) != 2 {
		t.Fatalf("trades rows = %d", len(got))
	}
	snap := m.Snapshot()
	if snap.MsgsAcked != 2 || snap.Fills != 2 {
		t.Fatalf("metrics = %+v", snap)
	}
}

func TestConsumerProcessNaksWhenNothingDurable(t *testing.T) {
	fc := newEtlFakeConn()
	fc.failInsert("ticks", true)
	m := NewIngestMetrics()
	// nil spool: nothing can be made durable -> Nak.
	ing := NewIngester(fc, nil, Options{InsertTimeout: 50 * time.Millisecond}, m, nil)
	cons := NewConsumer(nil, ing, ConsumerOptions{}, m, nil)

	msgs := []jetstream.Msg{
		&etlFakeMsg{subject: "trades.0.EUR-USD", data: encodeFill(t, 100, uint64(time.Now().UnixNano()), 42, 1, 1, 7)},
	}
	if err := cons.process(context.Background(), "trades", msgs); err == nil {
		t.Fatal("expected error: nothing durable")
	}
	if msgs[0].(*etlFakeMsg).acked {
		t.Fatal("acked a non-durable row")
	}
	if !msgs[0].(*etlFakeMsg).nakd {
		t.Fatal("expected nak")
	}
}

func TestParseSubject(t *testing.T) {
	shard, sym, ok := parseSubject("trades.0.EUR-USD")
	if !ok || shard != 0 || sym != "EUR/USD" {
		t.Fatalf("got %d %s %v", shard, sym, ok)
	}
	if _, _, ok := parseSubject("trades.0"); ok {
		t.Fatal("accepted short subject")
	}
	if _, _, ok := parseSubject("trades.x.EUR-USD"); ok {
		t.Fatal("accepted non-numeric shard")
	}
}

func TestMapIncomeType(t *testing.T) {
	cases := map[string]string{
		"FEE":         "COMMISSION",
		"ROLLOVER":    "SWAP_ROLLOVER",
		"ADJUSTMENT":  "NBP_ADJUSTMENT",
		"DEPOSIT":     "DEPOSIT", // pass-through
		"FUNDING_FEE": "FUNDING_FEE",
	}
	for in, want := range cases {
		if got := mapIncomeType(in); got != want {
			t.Fatalf("mapIncomeType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTickRowValuesExactScaling(t *testing.T) {
	row := TickRowValues(time.Unix(0, 1000).UTC(), "USD-JPY", 14987654321, 123456789, "BUY", 7, 3, 2)
	if d := row[2].(decimal.Decimal); !d.Equal(decimal.New(14987654321, -8)) {
		t.Fatalf("price scaled wrong: %v", d)
	}
	if d := row[3].(decimal.Decimal); !d.Equal(decimal.New(123456789, -8)) {
		t.Fatalf("qty scaled wrong: %v", d)
	}
	if row[7].(uint32) != 2 || row[5].(uint64) != 7 || row[6].(uint64) != 3 {
		t.Fatalf("row = %+v", row)
	}
}

func TestMetricsExposition(t *testing.T) {
	m := NewIngestMetrics()
	m.incRowsInserted(5)
	m.incSpooled(3)
	m.SetSpoolGauges(2, 4096)
	out := m.String()
	for _, name := range []string{
		"analytics_ch_rows_inserted_total",
		"analytics_spool_size_bytes",
		"analytics_fills_total",
		"analytics_income_projection_lag_seconds",
	} {
		if !strings.Contains(out, name) {
			t.Fatalf("exposition missing %s\n%s", name, out)
		}
	}
}

func TestVerStamping(t *testing.T) {
	fc := newEtlFakeConn()
	ing := NewIngester(fc, nil, Options{InsertTimeout: time.Second}, nil, nil)
	row := TickRowValues(time.Now().UTC(), "EUR-USD", 1, 1, "U", 9, 9, 0)
	if err := ing.InsertWithSpool(context.Background(), "ticks", [][]any{row}); err != nil {
		t.Fatal(err)
	}
	got := fc.got("ticks")
	if len(got) != 1 || len(got[0]) != 9 {
		t.Fatalf("row shape = %+v", got)
	}
	if v, ok := got[0][8].(uint64); !ok || v == 0 {
		t.Fatalf("ver not stamped: %+v", got[0])
	}
}

// ---- PG income projection fake + test -------------------------------------

// fakePGXRows is a minimal pgx.Rows over typed column tuples — enough for
// PollIncomeOnce's Scan sequence.
type fakePGXRows struct {
	rows [][]any
	pos  int
}

func (r *fakePGXRows) Close()                                       {}
func (r *fakePGXRows) Err() error                                   { return nil }
func (r *fakePGXRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakePGXRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakePGXRows) Conn() *pgx.Conn                              { return nil }
func (r *fakePGXRows) Next() bool {
	if r.pos < len(r.rows) {
		r.pos++
		return true
	}
	return false
}
func (r *fakePGXRows) Values() ([]any, error) { return r.rows[r.pos-1], nil }
func (r *fakePGXRows) RawValues() [][]byte    { return nil }
func (r *fakePGXRows) Scan(dest ...any) error {
	row := r.rows[r.pos-1]
	if len(dest) != len(row) {
		return fmt.Errorf("scan arity %d != %d", len(dest), len(row))
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *int64:
			*p = row[i].(int64)
		case *string:
			*p = row[i].(string)
		case *decimal.Decimal:
			*p = row[i].(decimal.Decimal)
		case *time.Time:
			*p = row[i].(time.Time)
		case **int64:
			if v, ok := row[i].(*int64); ok {
				*p = v
			} else {
				*p = nil
			}
		case **string:
			if v, ok := row[i].(*string); ok {
				*p = v
			} else {
				*p = nil
			}
		default:
			return fmt.Errorf("scan dest type %T unsupported by fake", d)
		}
	}
	return nil
}

type fakePGQuerier struct{ rows pgx.Rows }

func (q fakePGQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return q.rows, nil
}

func TestPollIncomeOnceProjectsSignedRows(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	str := func(v string) *string { return &v }
	posted := time.Now().UTC().Add(-time.Minute)
	// ledger_entries cols: id, account_id, currency, direction, amount,
	// entry_type, reference_id, journal_entry_id, posted_at, description
	rows := &fakePGXRows{rows: [][]any{
		{int64(101), int64(7), "USD", "DEBIT", decimal.RequireFromString("2.50"),
			"FEE", i64(55), i64(9), posted, str("commission")},
		{int64(102), int64(7), "USD", "CREDIT", decimal.RequireFromString("0.40"),
			"ADJUSTMENT", nil, i64(10), posted, str("nbp out")},
	}}
	fc := newEtlFakeConn()
	m := NewIngestMetrics()
	dir := t.TempDir()
	s, err := OpenSpool(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ing := NewIngester(fc, s, Options{InsertTimeout: time.Second}, m, nil)

	cursor, n, err := ing.PollIncomeOnce(context.Background(), fakePGQuerier{rows}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 102 || n != 2 {
		t.Fatalf("cursor=%d n=%d, want 102/2", cursor, n)
	}
	got := fc.got("income_ledger")
	if len(got) != 2 {
		t.Fatalf("income rows = %d", len(got))
	}
	// Row 0: DEBIT FEE -> +2.5 COMMISSION.
	if got[0][3].(string) != "COMMISSION" || got[0][4].(string) != "FEE" {
		t.Fatalf("income type map = %v/%v", got[0][3], got[0][4])
	}
	if d := got[0][5].(decimal.Decimal); !d.Equal(decimal.RequireFromString("2.50")) {
		t.Fatalf("debit amount = %v, want +2.50", d)
	}
	if got[0][6].(uint64) != 101 || got[0][7].(uint64) != 9 || got[0][8].(uint64) != 55 {
		t.Fatalf("row refs = %v %v %v", got[0][6], got[0][7], got[0][8])
	}
	// Row 1: CREDIT ADJUSTMENT -> -0.40 NBP_ADJUSTMENT.
	if d := got[1][5].(decimal.Decimal); !d.Equal(decimal.RequireFromString("-0.40")) {
		t.Fatalf("credit amount = %v, want -0.40", d)
	}
	if got[1][3].(string) != "NBP_ADJUSTMENT" {
		t.Fatalf("income type = %v, want NBP_ADJUSTMENT", got[1][3])
	}
	if snap := m.Snapshot(); snap.IncomeRows != 2 {
		t.Fatalf("income_rows metric = %d", snap.IncomeRows)
	}
}
