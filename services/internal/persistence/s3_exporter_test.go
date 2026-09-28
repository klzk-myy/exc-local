// Unit tests for Task 4.3.8 — ClickHouse→S3 daily batch exporter.
// Runs with no ClickHouse (fakeCH serves canned CSV) and no S3 (DirUploader
// writes to t.TempDir()).
package persistence

import (
	"archive/zip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeCH serves canned CSV bodies keyed on which dataset query arrived.
type fakeCH struct {
	symbolsCSV string // reply to the DISTINCT symbol discovery query
	bodies     map[Dataset]string
	queries    []string          // captured SQL for assertions
	failOn     map[Dataset]error // injected failures
}

func (f *fakeCH) QueryCSV(_ context.Context, q string) (io.ReadCloser, error) {
	f.queries = append(f.queries, q)
	for ds, err := range f.failOn {
		if err != nil && strings.Contains(q, string(tableFor(ds))) {
			return nil, err
		}
	}
	if strings.HasPrefix(q, "SELECT DISTINCT symbol") {
		return io.NopCloser(strings.NewReader(f.symbolsCSV)), nil
	}
	for ds, body := range f.bodies {
		if strings.Contains(q, "FROM "+tableFor(ds)) {
			return io.NopCloser(strings.NewReader(body)), nil
		}
	}
	return nil, fmt.Errorf("fakeCH: unmatched query: %s", q)
}

func tableFor(ds Dataset) string {
	s := DefaultSchema()
	switch ds {
	case DatasetTrades:
		return s.TradesTable
	case DatasetAggTrades:
		return s.AggTradesTable
	case DatasetKlines1m:
		return s.KlinesTable
	case DatasetBook:
		return s.BookTable
	}
	return ""
}

var day = time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)

func newExporter(t *testing.T, ch CHQuerier, nt Notifier) (*Exporter, DirUploader, string) {
	t.Helper()
	root := t.TempDir()
	up := DirUploader{Root: root}
	ex := NewExporter(ch, up, nt, Config{Schema: DefaultSchema()}, nil)
	return ex, up, root
}

func TestArchiveName(t *testing.T) {
	cases := []struct{ sym, ds, want string }{
		{"EUR/USD", "trades", "EURUSD-trades-2026-09-27.zip"},
		{"EURUSD", "aggTrades", "EURUSD-aggTrades-2026-09-27.zip"},
		{"GBP/JPY", "1m", "GBPJPY-1m-2026-09.zip"},
		{"USD-MXN", "bookSnapshots", "USDMXN-bookSnapshots-2026-09-27.zip"},
	}
	for _, c := range cases {
		if got := ArchiveName(c.sym, c.ds, day); got != c.want {
			t.Errorf("ArchiveName(%q,%q)=%q want %q", c.sym, c.ds, got, c.want)
		}
	}
}

func TestExportDayEndToEnd(t *testing.T) {
	ch := &fakeCH{
		symbolsCSV: "\"EUR/USD\"\n\"GBPUSD\"\n",
		bodies: map[Dataset]string{
			DatasetTrades: "timestamp,symbol,price,quantity,side,trade_id,shard_id\n" +
				"2026-09-27 00:00:01.000,\"EUR/USD\",1.1701,1000,buy,42,0\n",
			DatasetAggTrades: "agg_id,timestamp,symbol,price,quantity,first_trade_id,last_trade_id,side\n" +
				"7,2026-09-27 00:00:01.000,\"EUR/USD\",1.1701,1000,42,42,buy\n",
			DatasetKlines1m: "window_start,symbol,open,high,low,close,volume,trades_count\n" +
				"2026-09-27 00:00:00,\"EUR/USD\",1.17,1.171,1.169,1.1705,5000,3\n",
			DatasetBook: "timestamp,symbol,bid_price,bid_qty,ask_price,ask_qty\n" +
				"2026-09-27 00:00:01.000,\"EUR/USD\",1.1700,1000,1.1702,900\n",
		},
	}
	var notified *ExportResult
	ex, _, root := newExporter(t, ch, NotifyFunc(func(_ context.Context, e ExportResult) error {
		cp := e
		notified = &cp
		return nil
	}))
	res, err := ex.ExportDay(context.Background(), day)
	if err != nil {
		t.Fatalf("ExportDay: %v", err)
	}

	// 2 symbols × 4 datasets = 8 artifacts + manifest.
	if len(res.Artifacts) != 8 {
		t.Fatalf("artifacts=%d want 8", len(res.Artifacts))
	}
	if notified == nil || notified.ManifestKey != res.ManifestKey {
		t.Fatalf("completion event missing/incorrect: %+v", notified)
	}

	// Verify every uploaded ZIP on "disk": sha256 must match the manifest.
	for _, art := range res.Artifacts {
		data, err := readUploaded(root, art.Key)
		if err != nil {
			t.Fatalf("read upload %s: %v", art.Key, err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != art.SHA256 {
			t.Fatalf("%s: uploaded sha256 != manifest sha256", art.Key)
		}
	}

	// Read one zip properly + check manifest file on disk.
	tradesKey := "market-data/trades/EURUSD/EURUSD-trades-2026-09-27.zip"
	zf, err := zip.OpenReader(root + "/" + tradesKey)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zf.Close()
	if len(zf.File) != 1 || zf.File[0].Name != "EURUSD-trades-2026-09-27.csv" {
		t.Fatalf("zip contents wrong: %+v", zf.File[0].Name)
	}
	rc, _ := zf.File[0].Open()
	csv, _ := io.ReadAll(rc)
	rc.Close()
	if !strings.Contains(string(csv), "trade_id") ||
		!strings.Contains(string(csv), "1.1701") {
		t.Fatalf("csv body wrong: %s", csv)
	}

	man, err := readUploaded(root, res.ManifestKey)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m ExportResult
	if err := json.Unmarshal(man, &m); err != nil {
		t.Fatalf("manifest JSON: %v", err)
	}
	if m.Date != "2026-09-27" || len(m.Artifacts) != 8 {
		t.Fatalf("manifest content wrong: %s", man)
	}
	// Row counts: canned bodies have 1 data row each.
	for _, a := range m.Artifacts {
		if a.Rows != 1 {
			t.Fatalf("rows=%d want 1 for %+v", a.Rows, a)
		}
		if a.SHA256 == "" || a.MD5 == "" || a.Size == 0 {
			t.Fatalf("artifact checksums missing: %+v", a)
		}
	}
}

func TestExportDayFailClosed(t *testing.T) {
	ch := &fakeCH{
		symbolsCSV: "EURUSD\n",
		bodies: map[Dataset]string{
			DatasetTrades:    "h\nr\n",
			DatasetAggTrades: "h\nr\n",
			DatasetKlines1m:  "h\nr\n",
			DatasetBook:      "h\nr\n",
		},
		failOn: map[Dataset]error{DatasetAggTrades: fmt.Errorf("boom")},
	}
	ex, _, _ := newExporter(t, ch, nil)
	res, err := ex.ExportDay(context.Background(), day)
	if err == nil || res != nil {
		t.Fatalf("expected fail-closed abort, got res=%v err=%v", res, err)
	}
}

func TestQueryBuilders(t *testing.T) {
	ex, _, _ := newExporter(t, &fakeCH{}, nil)
	q := ex.datasetQuery(DatasetTrades, "EUR'USD", day)
	if !strings.Contains(q, `symbol = 'EUR\'USD'`) {
		t.Fatalf("symbol not escaped: %s", q)
	}
	if !strings.Contains(q, "toDate(timestamp) = '2026-09-27'") {
		t.Fatalf("day filter missing: %s", q)
	}
	qk := ex.datasetQuery(DatasetKlines1m, "EURUSD", day)
	if !strings.Contains(qk, "toYYYYMM(window_start) = toYYYYMM(toDate('2026-09-27'))") {
		t.Fatalf("month filter missing: %s", qk)
	}
}

func TestDirUploaderETagAndEscape(t *testing.T) {
	root := t.TempDir()
	up := DirUploader{Root: root}
	payload := []byte("hello")
	info, err := up.Put(context.Background(), "a/b.txt",
		strings.NewReader(string(payload)), int64(len(payload)), nil)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	want := md5.Sum(payload)
	if info.ETag != hex.EncodeToString(want[:]) {
		t.Fatalf("ETag=%q want %q", info.ETag, hex.EncodeToString(want[:]))
	}
	if _, err := up.Put(context.Background(), "../escape",
		strings.NewReader("x"), 1, nil); err == nil {
		t.Fatal("path traversal must be rejected")
	}
}

func readUploaded(root, key string) ([]byte, error) {
	return os.ReadFile(root + "/" + key)
}
