// Package persistence holds the ClickHouse-to-S3 daily batch exporter
// (Task 4.3.8, spec §16.1 / §24 #292).
//
// Once per UTC day (scheduled by cmd/s3-market-data-exporter at 01:00 UTC)
// the exporter streams the previous day's market data out of ClickHouse as
// CSV, packages it into per-symbol ZIP archives, uploads them to the public
// data bucket (data.{domain}) with a SHA256 manifest, and emits a completion
// event for the Phase-23 download catalog (Task 23.3.7).
//
// Archives (Binance-data layout):
//
//	{symbol}-trades-YYYY-MM-DD.zip        raw trade ticks, one CSV inside
//	{symbol}-aggTrades-YYYY-MM-DD.zip     aggregated trades
//	{symbol}-1m-YYYY-MM.zip               monthly 1m OHLCV klines, regenerated
//	                                      daily (idempotent overwrite)
//	{symbol}-bookSnapshots-YYYY-MM-DD.zip top-of-book snapshots
//	manifest-YYYY-MM-DD.json              SHA256/MD5/size/rows per artifact
//
// Both external dependencies are narrow seams so the exporter is unit-
// testable with no ClickHouse and no S3:
//   - CHQuerier  — returns the raw CSV body of a query (HTTP or native impl).
//   - Uploader   — object PUT; the shared objectstore package is owned by
//     another workstream, so s3/dir implementations live in the cmd wiring
//     and here (DirUploader, also used by dev and tests).
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
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// External seams
// ---------------------------------------------------------------------------

// CHQuerier streams a ClickHouse query result. The query MUST end with a
// FORMAT clause (the exporter builds queries with `FORMAT CSVWithNames` and
// pipes the body verbatim into the archive entry).
type CHQuerier interface {
	QueryCSV(ctx context.Context, query string) (io.ReadCloser, error)
}

// Uploader is the narrow object-store seam. Implementations live outside
// this package (shared objectstore ownership); Put streams `size` bytes of
// r to `key` and returns whatever the store reports (ETag for S3 PUTs).
type Uploader interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, meta map[string]string) (ObjectInfo, error)
}

// ObjectInfo is the post-PUT report. ETag may be empty for stores that do
// not return one (DirUploader synthesises an MD5 hex ETag anyway).
type ObjectInfo struct {
	ETag string
}

// Notifier emits the completion event for the Phase-23 catalog service.
// cmd wires NATS (subject analytics.marketdata.export.completed) or an HTTP
// callback; nil disables notification.
type Notifier interface {
	Notify(ctx context.Context, evt ExportResult) error
}

// ---------------------------------------------------------------------------
// Schema & config
// ---------------------------------------------------------------------------

// Schema names the ClickHouse tables the exporter reads. Column lists are
// fixed per dataset (below); table/column names follow spec §16.1/§16.2.
type Schema struct {
	Database       string // optional db qualifier, e.g. "market_data"
	TradesTable    string // spec §16.1 tick_history (ReplacingMergeTree)
	TradesTimeCol  string // default "timestamp"
	AggTradesTable string // aggregated trades (Phase-20 ingestion projection)
	KlinesTable    string // spec §16.2 ohlcv_1m (SummingMergeTree)
	KlinesTimeCol  string // default "window_start"
	BookTable      string // top-of-book depth snapshots
	BookTimeCol    string // default "timestamp"
}

// DefaultSchema returns the spec-conformant table set.
func DefaultSchema() Schema {
	return Schema{
		TradesTable:    "tick_history",
		TradesTimeCol:  "timestamp",
		AggTradesTable: "agg_trades",
		KlinesTable:    "ohlcv_1m",
		KlinesTimeCol:  "window_start",
		BookTable:      "book_snapshots",
		BookTimeCol:    "timestamp",
	}
}

func (s Schema) qualify(table string) string {
	if s.Database != "" {
		return s.Database + "." + table
	}
	return table
}

// Config controls one exporter run.
type Config struct {
	Schema Schema
	// Symbols restricts the export; empty means "every symbol with trades on
	// the day" (discovered via a DISTINCT query on TradesTable).
	Symbols []string
	// Prefix is the object-key root inside the bucket (default "market-data").
	Prefix string
	// IncludeBook exports top-of-book snapshots in addition to the three
	// spec-named datasets (default true — §16.1 requires the extraction).
	IncludeBook *bool
	// WorkDir holds temp files mid-export (default os.TempDir()).
	WorkDir string
}

// Artifact describes one uploaded archive.
type Artifact struct {
	Dataset string `json:"dataset"`
	Symbol  string `json:"symbol"`
	Name    string `json:"name"`
	Key     string `json:"key"`
	SHA256  string `json:"sha256"`
	MD5     string `json:"md5"`
	Size    int64  `json:"size"`
	Rows    int64  `json:"rows"`
}

// ExportResult is the manifest payload and the completion-event body.
type ExportResult struct {
	Date        string     `json:"date"`
	GeneratedAt time.Time  `json:"generated_at"`
	ManifestKey string     `json:"manifest_key"`
	Artifacts   []Artifact `json:"artifacts"`
}

// ---------------------------------------------------------------------------
// Exporter
// ---------------------------------------------------------------------------

type Exporter struct {
	ch  CHQuerier
	up  Uploader
	cfg Config
	nt  Notifier
	log *slog.Logger
}

func NewExporter(ch CHQuerier, up Uploader, nt Notifier, cfg Config, log *slog.Logger) *Exporter {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "market-data"
	}
	if cfg.Schema.TradesTable == "" {
		cfg.Schema = DefaultSchema()
	}
	return &Exporter{ch: ch, up: up, nt: nt, cfg: cfg, log: log}
}

// Dataset enumerates the four extracted data sets.
type Dataset string

const (
	DatasetTrades    Dataset = "trades"
	DatasetAggTrades Dataset = "aggTrades"
	DatasetKlines1m  Dataset = "1m"
	DatasetBook      Dataset = "bookSnapshots"
)

// FileSymbol turns "EUR/USD" into the filename-safe "EURUSD".
func FileSymbol(symbol string) string {
	return strings.ReplaceAll(strings.ReplaceAll(symbol, "/", ""), "-", "")
}

// ArchiveName is the public ZIP filename for (symbol, dataset, day).
func ArchiveName(symbol, dataset string, day time.Time) string {
	sym := FileSymbol(symbol)
	switch Dataset(dataset) {
	case DatasetKlines1m:
		return fmt.Sprintf("%s-1m-%s.zip", sym, day.Format("2006-01"))
	default:
		return fmt.Sprintf("%s-%s-%s.zip", sym, dataset, day.Format("2006-01-02"))
	}
}

// csvEntryName is the CSV filename embedded inside the ZIP.
func csvEntryName(symbol, dataset string, day time.Time) string {
	name := ArchiveName(symbol, dataset, day)
	return strings.TrimSuffix(name, ".zip") + ".csv"
}

// objectKey places archives under {prefix}/{dataset}/{symbol}/{name} and the
// manifest under {prefix}/manifest/.
func (e *Exporter) objectKey(symbol, dataset string, day time.Time) string {
	return fmt.Sprintf("%s/%s/%s/%s", e.cfg.Prefix, dataset, FileSymbol(symbol),
		ArchiveName(symbol, dataset, day))
}

func (e *Exporter) manifestKey(day time.Time) string {
	return fmt.Sprintf("%s/manifest/manifest-%s.json", e.cfg.Prefix,
		day.Format("2006-01-02"))
}

// quoteLit escapes a ClickHouse string literal (single quotes + backslashes).
func quoteLit(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + r.Replace(v) + "'"
}

// datasetQuery returns the CSVWithNames extraction SQL. Time filters use
// toDate()/toYYYYMM() on the configured time column; ORDER BY gives
// deterministic archives (byte-identical re-exports).
func (e *Exporter) datasetQuery(ds Dataset, symbol string, day time.Time) string {
	s := e.cfg.Schema
	sym := quoteLit(symbol)
	d := quoteLit(day.Format("2006-01-02"))
	switch ds {
	case DatasetTrades:
		return fmt.Sprintf(
			"SELECT %s, symbol, price, quantity, side, trade_id, shard_id FROM %s "+
				"WHERE toDate(%s) = %s AND symbol = %s ORDER BY %s FORMAT CSVWithNames",
			s.TradesTimeCol, s.qualify(s.TradesTable), s.TradesTimeCol, d, sym,
			s.TradesTimeCol)
	case DatasetAggTrades:
		return fmt.Sprintf(
			"SELECT agg_id, %s, symbol, price, quantity, first_trade_id, "+
				"last_trade_id, side FROM %s WHERE toDate(%s) = %s AND symbol = %s "+
				"ORDER BY %s FORMAT CSVWithNames",
			s.TradesTimeCol, s.qualify(s.AggTradesTable), s.TradesTimeCol, d, sym,
			s.TradesTimeCol)
	case DatasetKlines1m:
		return fmt.Sprintf(
			"SELECT %s, symbol, open, high, low, close, volume, trades_count FROM %s "+
				"WHERE toYYYYMM(%s) = toYYYYMM(toDate(%s)) AND symbol = %s "+
				"ORDER BY %s FORMAT CSVWithNames",
			s.KlinesTimeCol, s.qualify(s.KlinesTable), s.KlinesTimeCol, d, sym,
			s.KlinesTimeCol)
	case DatasetBook:
		return fmt.Sprintf(
			"SELECT %s, symbol, bid_price, bid_qty, ask_price, ask_qty FROM %s "+
				"WHERE toDate(%s) = %s AND symbol = %s ORDER BY %s FORMAT CSVWithNames",
			s.BookTimeCol, s.qualify(s.BookTable), s.BookTimeCol, d, sym,
			s.BookTimeCol)
	}
	return ""
}

// symbolsFor resolves the export symbol list: explicit config wins, else a
// DISTINCT query over the trades table for the day.
func (e *Exporter) symbolsFor(ctx context.Context, day time.Time) ([]string, error) {
	if len(e.cfg.Symbols) > 0 {
		return e.cfg.Symbols, nil
	}
	s := e.cfg.Schema
	q := fmt.Sprintf(
		"SELECT DISTINCT symbol FROM %s WHERE toDate(%s) = %s ORDER BY symbol FORMAT CSV",
		s.qualify(s.TradesTable), s.TradesTimeCol,
		quoteLit(day.Format("2006-01-02")))
	rc, err := e.ch.QueryCSV(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("discover symbols: %w", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("discover symbols: %w", err)
	}
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		line = strings.Trim(line, `"`)
		if line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

// lineCounter counts '\n' so the manifest can report row counts
// (CSV header excluded when CSVWithNames is used).
type lineCounter struct {
	w     io.Writer
	lines int64
}

func (c *lineCounter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	for _, b := range p[:n] {
		if b == '\n' {
			c.lines++
		}
	}
	return n, err
}

// exportOne extracts one (symbol, dataset, day) into a temp ZIP, hashes it
// while writing, uploads it, and verifies the store-reported checksum.
func (e *Exporter) exportOne(ctx context.Context, ds Dataset, symbol string,
	day time.Time) (Artifact, error) {
	art := Artifact{
		Dataset: string(ds), Symbol: symbol,
		Name: ArchiveName(symbol, string(ds), day),
		Key:  e.objectKey(symbol, string(ds), day),
	}

	tmp, err := os.CreateTemp(e.cfg.WorkDir, "exc-export-*.zip")
	if err != nil {
		return art, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	sha := sha256.New()
	md := md5.New()
	zw := zip.NewWriter(io.MultiWriter(tmp, sha, md))
	entry, err := zw.Create(csvEntryName(symbol, string(ds), day))
	if err != nil {
		tmp.Close()
		return art, fmt.Errorf("zip entry: %w", err)
	}

	rc, err := e.ch.QueryCSV(ctx, e.datasetQuery(ds, symbol, day))
	if err != nil {
		tmp.Close()
		return art, fmt.Errorf("query %s/%s: %w", ds, symbol, err)
	}
	lc := &lineCounter{w: entry}
	_, copyErr := io.Copy(lc, rc)
	closeErr := rc.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		tmp.Close()
		return art, fmt.Errorf("stream %s/%s: %w", ds, symbol, copyErr)
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		return art, fmt.Errorf("zip close: %w", err)
	}
	art.Rows = lc.lines - 1 // minus the CSVWithNames header line
	if art.Rows < 0 {
		art.Rows = 0
	}
	art.SHA256 = hex.EncodeToString(sha.Sum(nil))
	art.MD5 = hex.EncodeToString(md.Sum(nil))

	st, err := tmp.Stat()
	if err != nil {
		tmp.Close()
		return art, err
	}
	art.Size = st.Size()
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		return art, err
	}

	info, err := e.up.Put(ctx, art.Key, tmp, art.Size, map[string]string{
		"sha256": art.SHA256,
	})
	tmp.Close()
	if err != nil {
		return art, fmt.Errorf("upload %s: %w", art.Key, err)
	}

	// Checksum verification: S3 single-part PUT ETag is the payload MD5.
	// A store that returns no ETag is verified by the sha256 metadata +
	// manifest; one that returns a mismatched ETag means the bytes on the
	// wire were corrupted — fail closed.
	if etag := strings.Trim(info.ETag, `"`); etag != "" && etag != art.MD5 {
		return art, fmt.Errorf("upload %s: ETag %q != md5 %q",
			art.Key, info.ETag, art.MD5)
	}
	e.log.Info("exported archive", "key", art.Key, "rows", art.Rows,
		"size", art.Size, "sha256", art.SHA256[:12])
	return art, nil
}

// ExportDay runs the full batch for one UTC day: discover symbols, export
// all datasets for each, upload the manifest, then notify the catalog.
// Fail-closed: any dataset/upload error aborts the run (a partial manifest
// must never be published for the day).
func (e *Exporter) ExportDay(ctx context.Context, day time.Time) (*ExportResult, error) {
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	symbols, err := e.symbolsFor(ctx, day)
	if err != nil {
		return nil, err
	}
	datasets := []Dataset{DatasetTrades, DatasetAggTrades, DatasetKlines1m}
	if e.cfg.IncludeBook == nil || *e.cfg.IncludeBook {
		datasets = append(datasets, DatasetBook)
	}

	res := &ExportResult{
		Date:        day.Format("2006-01-02"),
		GeneratedAt: time.Now().UTC(),
	}
	for _, sym := range symbols {
		for _, ds := range datasets {
			art, err := e.exportOne(ctx, ds, sym, day)
			if err != nil {
				return nil, err
			}
			res.Artifacts = append(res.Artifacts, art)
		}
	}

	res.ManifestKey = e.manifestKey(day)
	manifest, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return nil, err
	}
	sha := sha256.Sum256(manifest)
	if _, err := e.up.Put(ctx, res.ManifestKey,
		strings.NewReader(string(manifest)), int64(len(manifest)),
		map[string]string{"sha256": hex.EncodeToString(sha[:])}); err != nil {
		return nil, fmt.Errorf("upload manifest: %w", err)
	}
	e.log.Info("export manifest uploaded", "key", res.ManifestKey,
		"artifacts", len(res.Artifacts))

	if e.nt != nil {
		if err := e.nt.Notify(ctx, *res); err != nil {
			// The export itself is durable; a failed notification is a
			// degraded-catalog event, not lost data — surface it, don't undo.
			e.log.Error("export completion notify failed",
				"error", err, "manifest", res.ManifestKey)
		}
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// DirUploader — local-directory Uploader (dev backend + tests)
// ---------------------------------------------------------------------------

// DirUploader implements Uploader by writing keys as files under Root.
// It synthesises an MD5-hex ETag so checksum verification runs identically
// to S3.
type DirUploader struct{ Root string }

func (d DirUploader) Put(ctx context.Context, key string, r io.Reader, _ int64,
	_ map[string]string) (ObjectInfo, error) {
	path := filepath.Join(d.Root, filepath.FromSlash(key))
	if !strings.HasPrefix(path, filepath.Clean(d.Root)+string(os.PathSeparator)) {
		return ObjectInfo{}, fmt.Errorf("key %q escapes upload root", key)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ObjectInfo{}, err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return ObjectInfo{}, err
	}
	md := md5.New()
	if _, err := io.Copy(io.MultiWriter(f, md), r); err != nil {
		f.Close()
		os.Remove(tmp)
		return ObjectInfo{}, err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return ObjectInfo{}, err
	}
	select {
	case <-ctx.Done():
		os.Remove(tmp)
		return ObjectInfo{}, ctx.Err()
	default:
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return ObjectInfo{}, err
	}
	return ObjectInfo{ETag: hex.EncodeToString(md.Sum(nil))}, nil
}

// ---------------------------------------------------------------------------
// NotifyFunc adapts a plain function to Notifier.
// ---------------------------------------------------------------------------

type NotifyFunc func(ctx context.Context, evt ExportResult) error

func (f NotifyFunc) Notify(ctx context.Context, evt ExportResult) error {
	return f(ctx, evt)
}
