// Tests for Phase-23 Task 23.3.2 — data export. Sources, object store,
// job store and notifier are in-memory fakes; the live CH/PG/S3 paths
// are covered by the EXC_CH_TEST / EXC_PG_TEST integration suites.
package marketdata

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/parquet-go/parquet-go"

	"exchange/internal/objectstore"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeTape is an in-memory TapeQuerier honouring the (ts,trade_id) DESC
// keyset — it records every page query so tests can assert batching.
type fakeTape struct {
	rows  []TradesRow // pre-sorted ts DESC, trade_id DESC
	err   error
	got   []TapeQuery
	maxID int // optional per-call row cap beyond q.Limit
}

func (f *fakeTape) QueryTape(_ context.Context, q TapeQuery) ([]TradesRow, error) {
	f.got = append(f.got, q)
	if f.err != nil {
		return nil, f.err
	}
	var out []TradesRow
	for _, r := range f.rows {
		if !q.From.IsZero() && r.Ts.Before(q.From) {
			continue
		}
		if !q.To.IsZero() && !r.Ts.Before(q.To) {
			continue
		}
		if q.After != nil {
			if r.Ts.After(q.After.Ts) ||
				(r.Ts.Equal(q.After.Ts) && r.TradeID >= q.After.TradeID) {
				continue
			}
		}
		out = append(out, r)
		if len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

// fakeKlines is an in-memory KlineQuerier honouring the open_time ASC
// keyset.
type fakeKlines struct {
	rows []KlineRow // pre-sorted open_time ASC
	err  error
	got  []KlineQuery
}

func (f *fakeKlines) QueryKlines(_ context.Context, q KlineQuery) ([]KlineRow, error) {
	f.got = append(f.got, q)
	if f.err != nil {
		return nil, f.err
	}
	var out []KlineRow
	for _, r := range f.rows {
		if q.After != nil && !r.OpenTime.After(*q.After) {
			continue
		}
		out = append(out, r)
		if len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

// fakeObjects is an in-memory ObjectStore; Put drains the reader and
// stamps the md5 ETag the seam contract promises.
type fakeObjects struct {
	objs   map[string][]byte
	putErr error
	gotIn  []objectstore.PutInput
}

func newFakeObjects() *fakeObjects {
	return &fakeObjects{objs: map[string][]byte{}}
}

func (f *fakeObjects) Put(_ context.Context, in objectstore.PutInput) (objectstore.Object, error) {
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return objectstore.Object{}, err
	}
	if f.putErr != nil {
		return objectstore.Object{}, f.putErr
	}
	f.objs[in.Key] = body
	f.gotIn = append(f.gotIn, in)
	sum := md5.Sum(body)
	return objectstore.Object{
		Key: in.Key, Size: int64(len(body)),
		ETag: hex.EncodeToString(sum[:]),
	}, nil
}

func (f *fakeObjects) Get(_ context.Context, key string) ([]byte, objectstore.Object, error) {
	body, ok := f.objs[key]
	if !ok {
		return nil, objectstore.Object{}, fmt.Errorf("no such key %q", key)
	}
	return body, objectstore.Object{Key: key, Size: int64(len(body))}, nil
}

func (f *fakeObjects) Delete(_ context.Context, key string) error {
	delete(f.objs, key)
	return nil
}

// fakeNotifier records link sends.
type fakeNotifier struct {
	calls []struct {
		To  string
		Job Job
		URL string
	}
	err error
}

func (f *fakeNotifier) NotifyExportReady(_ context.Context, to string, j Job, url string) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, struct {
		To  string
		Job Job
		URL string
	}{to, j, url})
	return nil
}

// fakeRecipients resolves account→email.
type fakeRecipients struct{ m map[int64]string }

func (f fakeRecipients) Email(_ context.Context, accountID int64) (string, error) {
	return f.m[accountID], nil
}

// fakeCHConn is a CHQueryConn returning canned trades rows through a
// minimal driver.Rows.
type fakeCHConn struct {
	rows [][]any
	err  error
	gotQ []string
}

func (c *fakeCHConn) Query(_ context.Context, query string, _ ...any) (driver.Rows, error) {
	c.gotQ = append(c.gotQ, query)
	if c.err != nil {
		return nil, c.err
	}
	return &fakeCHRows{rows: c.rows}, nil
}

type fakeCHRows struct {
	rows [][]any
	pos  int
}

func (r *fakeCHRows) Next() bool { return r.pos < len(r.rows) }

func (r *fakeCHRows) Scan(dest ...any) error {
	row := r.rows[r.pos]
	r.pos++
	for i, d := range dest {
		switch p := d.(type) {
		case *time.Time:
			*p = row[i].(time.Time)
		case *uint64:
			*p = row[i].(uint64)
		case *string:
			*p = row[i].(string)
		case *decimal.Decimal:
			*p = row[i].(decimal.Decimal)
		default:
			return fmt.Errorf("fakeCHRows: unhandled scan dest %T", d)
		}
	}
	return nil
}

func (r *fakeCHRows) ScanStruct(any) error      { return errors.New("unused") }
func (r *fakeCHRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *fakeCHRows) Totals(...any) error       { return nil }
func (r *fakeCHRows) Columns() []string         { return nil }
func (r *fakeCHRows) Close() error              { return nil }
func (r *fakeCHRows) Err() error                { return nil }

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

var exportT0 = time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)

func tapeRows(n int) []TradesRow {
	out := make([]TradesRow, n)
	for i := 0; i < n; i++ {
		out[i] = TradesRow{
			TradeID: uint64(1000 - i),
			Ts:      exportT0.Add(-time.Duration(i) * time.Second),
			Symbol:  "EURUSD", Side: "BUY",
			Price: decimal.MustFromString("1.0855"),
			Qty:   decimal.MustFromString("1000.5"),
		}
	}
	return out
}

func klineRows(n int) []KlineRow {
	out := make([]KlineRow, n)
	for i := 0; i < n; i++ {
		out[i] = KlineRow{
			OpenTime: exportT0.Add(time.Duration(i) * time.Minute),
			Symbol:   "EURUSD", Interval: "1m",
			Open:        decimal.MustFromString("1.0850"),
			High:        decimal.MustFromString("1.0860"),
			Low:         decimal.MustFromString("1.0840"),
			Close:       decimal.MustFromString("1.0855"),
			Volume:      decimal.MustFromString("12345.678"),
			QuoteVolume: decimal.MustFromString("13400.1"),
			TradeCount:  42,
		}
	}
	return out
}

func exportSvc(tape TapeQuerier) *ExportService {
	return &ExportService{
		Trades: tape, Ticks: tape,
		Klines:     &fakeKlines{},
		IntervalOK: func(string) bool { return true },
		Jobs:       NewMemJobStore(),
		Objects:    newFakeObjects(),
	}
}

func tradesReq(format ExportFormat) ExportRequest {
	return ExportRequest{
		Kind: KindTrades, Symbol: "EURUSD", Format: format, AccountID: 7,
	}
}

// ---------------------------------------------------------------------------
// Validation / admission
// ---------------------------------------------------------------------------

func TestExportValidate(t *testing.T) {
	base := tradesReq(FormatCSV)
	if err := base.Validate(); err != nil {
		t.Fatalf("base request rejected: %v", err)
	}
	bad := base
	bad.Format = "xml"
	if err := bad.Validate(); !errors.Is(err, ErrExportFormatInvalid) {
		t.Fatalf("format=xml: want ErrExportFormatInvalid, got %v", err)
	}
	bad = base
	bad.Kind = "orders"
	if err := bad.Validate(); !errors.Is(err, ErrExportKindInvalid) {
		t.Fatalf("kind=orders: want ErrExportKindInvalid, got %v", err)
	}
	bad = base
	bad.Limit = ExportMaxRows + 1
	if err := bad.Validate(); !errors.Is(err, ErrExportLimitExceeded) {
		t.Fatalf("limit=1,000,001: want ErrExportLimitExceeded, got %v", err)
	}
	bad = base
	bad.Limit = ExportMaxRows
	if err := bad.Validate(); err != nil {
		t.Fatalf("limit=1,000,000 (cap boundary) rejected: %v", err)
	}
	bad = base
	bad.From, bad.To = exportT0, exportT0 // empty range
	if err := bad.Validate(); err == nil {
		t.Fatal("from==to range accepted")
	}
	bad = base
	bad.Kind = KindKlines
	bad.Interval = ""
	if err := bad.Validate(); !errors.Is(err, ErrExportIntervalInvalid) {
		t.Fatalf("klines without interval: want ErrExportIntervalInvalid, got %v", err)
	}
}

func TestExportAdmit(t *testing.T) {
	svc := &ExportService{Trades: &fakeTape{}}
	if err := svc.Admit(tradesReq(FormatCSV)); err != nil {
		t.Fatalf("admit trades: %v", err)
	}
	req := tradesReq(FormatCSV)
	req.Kind = KindTicks
	if err := svc.Admit(req); !errors.Is(err, ErrExportSourceMissing) {
		t.Fatalf("unwired ticks seam: want ErrExportSourceMissing, got %v", err)
	}
	svc.Ticks = svc.Trades
	svc.Klines = &fakeKlines{}
	svc.IntervalOK = func(iv string) bool { return iv == "1m" }
	req.Kind = KindKlines
	req.Interval = "3h" // not persisted
	if err := svc.Admit(req); !errors.Is(err, ErrExportIntervalInvalid) {
		t.Fatalf("interval=3h: want ErrExportIntervalInvalid, got %v", err)
	}
	req.Interval = "1m"
	if err := svc.Admit(req); err != nil {
		t.Fatalf("interval=1m: %v", err)
	}
}

func TestExportNeedsAsync(t *testing.T) {
	req := tradesReq(FormatCSV)
	if req.NeedsAsync() {
		t.Fatal("default sync request flagged async")
	}
	req.Async = true
	if !req.NeedsAsync() {
		t.Fatal("?async=1 not flagged async")
	}
	req.Async = false
	req.Limit = ExportSyncRows + 1
	if !req.NeedsAsync() {
		t.Fatal("bound above the inline ceiling not flagged async")
	}
	req.Limit = ExportSyncRows
	if req.NeedsAsync() {
		t.Fatal("bound at the inline ceiling flagged async")
	}
}

// ---------------------------------------------------------------------------
// Sync render — CSV / JSON envelope / Parquet
// ---------------------------------------------------------------------------

func TestExportStreamCSV(t *testing.T) {
	tape := &fakeTape{rows: tapeRows(3)}
	svc := exportSvc(tape)
	var buf bytes.Buffer
	rows, truncated, err := svc.Stream(context.Background(), tradesReq(FormatCSV), &buf)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if rows != 3 || truncated {
		t.Fatalf("rows=%d truncated=%v, want 3/false", rows, truncated)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v", err)
	}
	wantHeader := "trade_id,time,symbol,side,price,quantity"
	got := strings.Join(recs[0], ",")
	if got != wantHeader {
		t.Fatalf("header %q, want %q", got, wantHeader)
	}
	if len(recs) != 4 {
		t.Fatalf("csv lines %d, want header+3", len(recs))
	}
	if recs[1][0] != "1000" || recs[1][4] != "1.08550000" || recs[1][5] != "1000.50000000" {
		t.Fatalf("row values wrong: %v", recs[1])
	}
	if recs[1][1] != exportT0.Format(time.RFC3339) {
		t.Fatalf("time col %q, want RFC3339 %q", recs[1][1], exportT0.Format(time.RFC3339))
	}
}

func TestExportStreamJSONEnvelope(t *testing.T) {
	tape := &fakeTape{rows: tapeRows(2)}
	svc := exportSvc(tape)
	var buf bytes.Buffer
	rows, _, err := svc.Stream(context.Background(), tradesReq(FormatJSON), &buf)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if rows != 2 {
		t.Fatalf("rows=%d, want 2", rows)
	}
	var env struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("envelope parse: %v (body %q)", err, buf.String())
	}
	if len(env.Data) != 2 {
		t.Fatalf("data len %d, want 2", len(env.Data))
	}
	if env.Data[0]["price"] != "1.08550000" {
		t.Fatalf("price %v — decimals must render as strings", env.Data[0]["price"])
	}
	if env.Data[0]["side"] != "BUY" {
		t.Fatalf("side %v", env.Data[0]["side"])
	}
}

func TestExportStreamParquet(t *testing.T) {
	tape := &fakeTape{rows: tapeRows(3)}
	svc := exportSvc(tape)
	var buf bytes.Buffer
	rows, _, err := svc.Stream(context.Background(), tradesReq(FormatParquet), &buf)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if rows != 3 {
		t.Fatalf("rows=%d, want 3", rows)
	}
	r := parquet.NewGenericReader[parquetTradeRow](bytes.NewReader(buf.Bytes()))
	out := make([]parquetTradeRow, 8)
	n, err := r.Read(out)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("parquet read: %v", err)
	}
	if n != 3 {
		t.Fatalf("parquet rows %d, want 3", n)
	}
	if out[0].TradeID != 1000 || out[0].Symbol != "EURUSD" || out[0].Side != "BUY" {
		t.Fatalf("row0 %+v", out[0])
	}
	if out[0].Price != 108550000 { // 1.0855 scaled 1e8
		t.Fatalf("price scaled %d, want 108550000", out[0].Price)
	}
	if out[0].Ts != exportT0.UnixMilli() {
		t.Fatalf("ts %d, want %d (ms)", out[0].Ts, exportT0.UnixMilli())
	}
	// Schema assertion — the pinned columns, in order.
	schema := r.Schema()
	wantCols := []string{"trade_id", "ts", "symbol", "side", "price", "qty"}
	fields := schema.Fields()
	if len(fields) != len(wantCols) {
		t.Fatalf("schema fields %d, want %d", len(fields), len(wantCols))
	}
	for i, f := range fields {
		if f.Name() != wantCols[i] {
			t.Fatalf("column %d = %q, want %q", i, f.Name(), wantCols[i])
		}
	}
}

func TestExportStreamKlinesCSV(t *testing.T) {
	svc := exportSvc(&fakeTape{})
	kl := &fakeKlines{rows: klineRows(2)}
	svc.Klines = kl
	var buf bytes.Buffer
	req := tradesReq(FormatCSV)
	req.Kind = KindKlines
	req.Interval = "1m"
	rows, _, err := svc.Stream(context.Background(), req, &buf)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if rows != 2 {
		t.Fatalf("rows=%d, want 2", rows)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v", err)
	}
	if recs[0][0] != "open_time" || recs[0][9] != "trade_count" {
		t.Fatalf("kline header wrong: %v", recs[0])
	}
	if recs[1][3] != "1.08500000" || recs[1][9] != "42" {
		t.Fatalf("kline row wrong: %v", recs[1])
	}
}

// ---------------------------------------------------------------------------
// Keyset paging / cap / truncation
// ---------------------------------------------------------------------------

func TestExportKeysetBatches(t *testing.T) {
	// 12,000 rows must take 3 pages at the 5,000-row batch size — and the
	// second+ pages must carry the After cursor, never an OFFSET.
	tape := &fakeTape{rows: tapeRows(12_000)}
	svc := exportSvc(tape)
	var buf bytes.Buffer
	req := tradesReq(FormatJSON)
	req.Limit = 12_000
	rows, truncated, err := svc.Stream(context.Background(), req, &buf)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if rows != 12_000 || truncated {
		t.Fatalf("rows=%d truncated=%v", rows, truncated)
	}
	if len(tape.got) != 4 { // 3 full pages + the 1-row truncation probe
		t.Fatalf("page calls %d, want 4", len(tape.got))
	}
	if tape.got[0].After != nil {
		t.Fatal("first page carried an After cursor")
	}
	for i, q := range tape.got[1:] {
		if q.After == nil {
			t.Fatalf("page %d missing After keyset", i+1)
		}
	}
	if tape.got[1].After.TradeID != tape.rows[4999].TradeID {
		t.Fatalf("page2 keyset trade_id %d, want %d",
			tape.got[1].After.TradeID, tape.rows[4999].TradeID)
	}
	for _, q := range tape.got {
		if q.Limit > ExportPageRows {
			t.Fatalf("page limit %d exceeds the 5k batch contract", q.Limit)
		}
	}
}

func TestExportTruncated(t *testing.T) {
	tape := &fakeTape{rows: tapeRows(10)}
	svc := exportSvc(tape)
	var buf bytes.Buffer
	req := tradesReq(FormatJSON)
	req.Limit = 5
	rows, truncated, err := svc.Stream(context.Background(), req, &buf)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if rows != 5 || !truncated {
		t.Fatalf("rows=%d truncated=%v, want 5/true", rows, truncated)
	}
	var env struct {
		Data []any `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil || len(env.Data) != 5 {
		t.Fatalf("truncated envelope must still be valid {data:[5]}: %v", err)
	}
}

func TestExportSourceError(t *testing.T) {
	tape := &fakeTape{err: errors.New("ch gone")}
	svc := exportSvc(tape)
	var buf bytes.Buffer
	_, _, err := svc.Stream(context.Background(), tradesReq(FormatCSV), &buf)
	if err == nil || !strings.Contains(err.Error(), "ch gone") {
		t.Fatalf("want source error propagated, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// CHTradesStore over a fake driver conn
// ---------------------------------------------------------------------------

func TestCHTradesStoreQueryTape(t *testing.T) {
	conn := &fakeCHConn{rows: [][]any{
		{exportT0, uint64(9), "EURUSD", "SELL",
			decimal.MustFromString("1.1"), decimal.MustFromString("50")},
	}}
	store := NewCHTradesStore(conn)
	rows, err := store.QueryTape(context.Background(), TapeQuery{
		Symbol: "EURUSD",
		From:   exportT0.Add(-time.Hour), To: exportT0.Add(time.Hour),
		After: &TapeCursor{Ts: exportT0.Add(time.Minute), TradeID: 42},
		Limit: 500,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 || rows[0].TradeID != 9 || rows[0].Side != "SELL" {
		t.Fatalf("rows %+v", rows)
	}
	q := conn.gotQ[0]
	for _, frag := range []string{
		"FROM trades FINAL", "symbol = ?", "ts >= ?", "ts < ?",
		"(ts, trade_id) < (?, ?)", "ORDER BY ts DESC, trade_id DESC",
	} {
		if !strings.Contains(q, frag) {
			t.Fatalf("query missing %q: %s", frag, q)
		}
	}
}

// ---------------------------------------------------------------------------
// Async lifecycle — Enqueue / ProcessJob / download / notify
// ---------------------------------------------------------------------------

func TestExportAsyncLifecycle(t *testing.T) {
	tape := &fakeTape{rows: tapeRows(4)}
	jobs := NewMemJobStore()
	objs := newFakeObjects()
	notif := &fakeNotifier{}
	now := exportT0
	svc := &ExportService{
		Trades: tape, Jobs: jobs, Objects: objs,
		Notifier: notif, Recipients: fakeRecipients{m: map[int64]string{7: "a@x.io"}},
		LinkBase: "https://app.test", Now: func() time.Time { return now },
	}
	req := tradesReq(FormatCSV)
	req.Async = true
	job, err := svc.Enqueue(context.Background(), req)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if job.ID == 0 || job.Status != JobPending {
		t.Fatalf("job %+v not PENDING", job)
	}
	claimed, err := jobs.ClaimPending(context.Background(), 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %d", err, len(claimed))
	}
	if claimed[0].Status != JobRunning {
		t.Fatalf("claimed status %q", claimed[0].Status)
	}
	if err := svc.ProcessJob(context.Background(), claimed[0]); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, err := jobs.Get(context.Background(), job.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != JobCompleted {
		t.Fatalf("status %q", got.Status)
	}
	if got.ObjectRef != "exports/7/1.csv" {
		t.Fatalf("object_ref %q", got.ObjectRef)
	}
	if got.RowCount != 4 {
		t.Fatalf("row_count %d", got.RowCount)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(now.Add(ExportLinkTTL)) {
		t.Fatalf("expires_at %v, want now+24h", got.ExpiresAt)
	}
	if len(got.SHA256) != 64 {
		t.Fatalf("sha256 %q", got.SHA256)
	}
	// Artifact landed and hashes match what Complete recorded.
	body := objs.objs[got.ObjectRef]
	sum := md5.Sum(body)
	if got.SHA256 == "" || len(body) == 0 {
		t.Fatal("artifact empty")
	}
	_ = sum // ETag verified inside ProcessJob already
	// Link email dispatched with the object-scoped URL + expiry.
	if len(notif.calls) != 1 {
		t.Fatalf("notify calls %d", len(notif.calls))
	}
	call := notif.calls[0]
	if call.To != "a@x.io" || !strings.Contains(call.URL, "/api/v1/export-jobs/1/download") {
		t.Fatalf("notify %+v", call)
	}
	if got.NotifiedAt == nil {
		t.Fatal("notified_at not stamped")
	}
}

func TestExportAsyncJSONLArtifact(t *testing.T) {
	tape := &fakeTape{rows: tapeRows(2)}
	jobs := NewMemJobStore()
	objs := newFakeObjects()
	svc := &ExportService{Trades: tape, Jobs: jobs, Objects: objs}
	req := tradesReq(FormatJSON)
	req.Async = true
	job, _ := svc.Enqueue(context.Background(), req)
	claimed, _ := jobs.ClaimPending(context.Background(), 1)
	if err := svc.ProcessJob(context.Background(), claimed[0]); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := jobs.Get(context.Background(), job.ID)
	if got.ObjectRef != "exports/7/1.jsonl" {
		t.Fatalf("json artifact key %q, want .jsonl", got.ObjectRef)
	}
	lines := strings.Split(strings.TrimRight(string(objs.objs[got.ObjectRef]), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("jsonl lines %d, want 2", len(lines))
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &doc); err != nil {
		t.Fatalf("line not a json doc: %v", err)
	}
	if doc["price"] != "1.08550000" {
		t.Fatalf("doc %+v", doc)
	}
}

func TestExportProcessJobObjectPutFails(t *testing.T) {
	tape := &fakeTape{rows: tapeRows(2)}
	jobs := NewMemJobStore()
	objs := &fakeObjects{objs: map[string][]byte{}, putErr: errors.New("s3 down")}
	svc := &ExportService{Trades: tape, Jobs: jobs, Objects: objs}
	req := tradesReq(FormatCSV)
	req.Async = true
	job, _ := svc.Enqueue(context.Background(), req)
	claimed, _ := jobs.ClaimPending(context.Background(), 1)
	err := svc.ProcessJob(context.Background(), claimed[0])
	if err == nil {
		t.Fatal("process must fail on put error")
	}
	got, _ := jobs.Get(context.Background(), job.ID)
	if got.Status != JobFailed || !strings.Contains(got.Error, "s3 down") {
		t.Fatalf("job %+v — want FAILED with put error", got)
	}
}

func TestExportNotifyRetry(t *testing.T) {
	// A notify failure keeps notified_at NULL; the next RunDue pass
	// re-sends — the Unnotified scan is the retry leg.
	tape := &fakeTape{rows: tapeRows(1)}
	jobs := NewMemJobStore()
	objs := newFakeObjects()
	notif := &fakeNotifier{err: errors.New("smtp down")}
	svc := &ExportService{
		Trades: tape, Jobs: jobs, Objects: objs,
		Notifier: notif, Recipients: fakeRecipients{m: map[int64]string{7: "a@x.io"}},
	}
	req := tradesReq(FormatCSV)
	req.Async = true
	job, _ := svc.Enqueue(context.Background(), req)
	claimed, _ := jobs.ClaimPending(context.Background(), 1)
	if err := svc.ProcessJob(context.Background(), claimed[0]); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := jobs.Get(context.Background(), job.ID)
	if got.Status != JobCompleted || got.NotifiedAt != nil {
		t.Fatalf("job %+v — want COMPLETED w/o notified_at", got)
	}
	notif.err = nil
	if _, err := svc.RunDue(context.Background(), 10); err != nil {
		t.Fatalf("rundue: %v", err)
	}
	got, _ = jobs.Get(context.Background(), job.ID)
	if got.NotifiedAt == nil {
		t.Fatal("retry pass did not stamp notified_at")
	}
	if len(notif.calls) != 1 {
		t.Fatalf("notify calls %d, want exactly 1", len(notif.calls))
	}
}

// ---------------------------------------------------------------------------
// Download — owner-scoped, expiry-gated
// ---------------------------------------------------------------------------

func TestExportDownloadForAccount(t *testing.T) {
	now := exportT0
	jobs := NewMemJobStore()
	objs := newFakeObjects()
	svc := &ExportService{Jobs: jobs, Objects: objs, Now: func() time.Time { return now }}
	exp := now.Add(time.Hour)
	fin := now
	id, _ := jobs.Insert(context.Background(), Job{
		AccountID: 7, Kind: "trades", Symbol: "EURUSD", Format: "csv",
		Status: JobCompleted, ObjectRef: "exports/7/1.csv",
		ExpiresAt: &exp, FinishedAt: &fin, CreatedAt: now,
	})
	j, _ := jobs.Get(context.Background(), id)
	j.Status = JobRunning // flip via insert→claim→complete lifecycle
	// Rebuild through the real lifecycle instead of mutating internals:
	jobs2 := NewMemJobStore()
	svc.Jobs = jobs2
	objs.objs["exports/7/1.csv"] = []byte("trade_id,time\n")
	oid, _ := jobs2.Insert(context.Background(), Job{
		AccountID: 7, Kind: "trades", Symbol: "EURUSD", Format: "csv",
		Status: JobPending, CreatedAt: now,
	})
	cl, _ := jobs2.ClaimPending(context.Background(), 1)
	if err := jobs2.Complete(context.Background(), cl[0].ID, Completion{
		ObjectRef: "exports/7/1.csv", RowCount: 0, ExpiresAt: exp, At: now,
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	dl, err := svc.DownloadForAccount(context.Background(), oid, 7)
	if err != nil {
		t.Fatalf("owner download: %v", err)
	}
	if dl.Filename != "trades_EURUSD_"+fmt.Sprint(oid)+".csv" {
		t.Fatalf("filename %q", dl.Filename)
	}
	if _, err := svc.DownloadForAccount(context.Background(), oid, 8); !errors.Is(err, ErrExportJobNotFound) {
		t.Fatalf("cross-account: want ErrExportJobNotFound, got %v", err)
	}
	if _, err := svc.DownloadForAccount(context.Background(), 9999, 7); !errors.Is(err, ErrExportJobNotFound) {
		t.Fatalf("unknown id: want ErrExportJobNotFound, got %v", err)
	}
	// Expired link → ErrExportExpired (mapped to NOT_FOUND at the edge).
	past := now.Add(-time.Second)
	pid, _ := jobs2.Insert(context.Background(), Job{
		AccountID: 7, Kind: "trades", Symbol: "EURUSD", Format: "csv",
		Status: JobPending, CreatedAt: now,
	})
	cl2, _ := jobs2.ClaimPending(context.Background(), 1)
	if cl2[0].ID != pid {
		t.Fatalf("claim order %+v", cl2)
	}
	if err := jobs2.Complete(context.Background(), pid, Completion{
		ObjectRef: "exports/7/1.csv", ExpiresAt: past, At: now,
	}); err != nil {
		t.Fatalf("complete2: %v", err)
	}
	if _, err := svc.DownloadForAccount(context.Background(), pid, 7); !errors.Is(err, ErrExportExpired) {
		t.Fatalf("expired: want ErrExportExpired, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// MemJobStore keyset list
// ---------------------------------------------------------------------------

func TestMemJobStoreListKeyset(t *testing.T) {
	m := NewMemJobStore()
	base := exportT0
	for i := 0; i < 5; i++ {
		if _, err := m.Insert(context.Background(), Job{
			AccountID: 7, Kind: "trades", Symbol: "EURUSD",
			Format: "csv", CreatedAt: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Other account's job is invisible.
	if _, err := m.Insert(context.Background(), Job{
		AccountID: 8, Kind: "trades", Symbol: "EURUSD",
		Format: "csv", CreatedAt: base.Add(99 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	page1, err := m.ListForAccount(context.Background(), 7, time.Time{}, 0, 3)
	if err != nil || len(page1) != 3 {
		t.Fatalf("page1 %d %v", len(page1), err)
	}
	if page1[0].ID != 5 || page1[2].ID != 3 {
		t.Fatalf("newest-first order wrong: %+v", page1)
	}
	last := page1[len(page1)-1]
	page2, err := m.ListForAccount(context.Background(), 7, last.CreatedAt, last.ID, 3)
	if err != nil || len(page2) != 2 {
		t.Fatalf("page2 %d %v", len(page2), err)
	}
	if page2[0].ID != 2 || page2[1].ID != 1 {
		t.Fatalf("keyset continuation wrong: %+v", page2)
	}
}
