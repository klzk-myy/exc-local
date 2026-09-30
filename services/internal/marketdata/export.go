// Phase-23 Task 23.3.2 — data export service (spec §10, §16.6, §2.7).
//
// Exports the ClickHouse cold-tier projections (trades / ticks / klines —
// the same tables the Phase-20 history endpoints read) in CSV, JSON and
// Parquet. Two execution paths share one keyset pager:
//
//   - sync:  ≤ ExportSyncRows rows are rendered inline by the REST
//            handler (JSON emits the §8.8-style {data:[...]} envelope;
//            CSV/Parquet stream as attachment downloads).
//   - async: explicit ?async=1, or a bound above ExportSyncRows, enqueues
//            an export_jobs row (migration 256); a worker drains it in
//            ExportPageRows keyset batches into an S3 object, stamps the
//            job COMPLETED with the object ref + 24h link expiry, and
//            emails the download link via the Task-20.3.8 delivery seam.
//
// Hard cap: ExportMaxRows (1,000,000) rows per export — the admission
// guard maps to EXPORT_LIMIT_EXCEEDED.
//
// Data-scope note: the rendered rows are the public tape projection only
// (trade_id/ts/symbol/side/price/quantity, or OHLCV for klines). The
// trades table's maker/taker account columns are never selected —
// account linkage never leaves ClickHouse.
package marketdata

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"exchange/internal/objectstore"
	"exchange/internal/reporting"
	"exchange/pkg/decimal"
)

// ExportKind selects the archived dataset.
type ExportKind string

const (
	KindTrades ExportKind = "trades" // exchange_analytics.trades
	KindTicks  ExportKind = "ticks"  // exchange_analytics.ticks
	KindKlines ExportKind = "klines" // exchange_analytics.ohlcv_{interval}
)

// ExportFormat selects the render codec.
type ExportFormat string

const (
	FormatCSV     ExportFormat = "csv"
	FormatJSON    ExportFormat = "json"
	FormatParquet ExportFormat = "parquet"
)

const (
	// ExportMaxRows is the spec §10/§16.6 hard cap — 1,000,000 rows per
	// export; admission rejects beyond it with EXPORT_LIMIT_EXCEEDED.
	ExportMaxRows = 1_000_000
	// ExportSyncRows is the inline ceiling — a bound above it must run
	// async (or the caller asked for async explicitly).
	ExportSyncRows = 50_000
	// ExportPageRows is the ClickHouse keyset batch size — rows are
	// never buffered past one page (§16.6 memory contract).
	ExportPageRows = 5_000
	// ExportParquetGroupRows bounds one parquet row group before the
	// writer flushes to the output stream.
	ExportParquetGroupRows = 50_000
)

// ExportLinkTTL bounds how long the emailed/stored download link stays
// valid — after expiry the download endpoint refuses (fail-closed).
const ExportLinkTTL = 24 * time.Hour

// Job status lattice (migration 256 CHECK constraint mirrors this).
const (
	JobPending   = "PENDING"
	JobRunning   = "RUNNING"
	JobCompleted = "COMPLETED"
	JobFailed    = "FAILED"
)

// Admission/service errors — the api layer maps them onto the §23 codes
// EXPORT_FORMAT_INVALID, EXPORT_LIMIT_EXCEEDED, EXPORT_JOB_NOT_FOUND,
// INVALID_REQUEST and SERVICE_DEGRADED.
var (
	ErrExportKindInvalid     = errors.New("marketdata: unknown export kind")
	ErrExportFormatInvalid   = errors.New("marketdata: unknown export format")
	ErrExportIntervalInvalid = errors.New("marketdata: kline interval is not a persisted timeframe")
	ErrExportLimitExceeded   = errors.New("marketdata: export exceeds the 1,000,000-row cap")
	ErrExportJobNotFound     = errors.New("marketdata: export job not found")
	ErrExportNotCompleted    = errors.New("marketdata: export job is not completed")
	ErrExportExpired         = errors.New("marketdata: export download link expired")
	ErrExportSourceMissing   = errors.New("marketdata: export source not configured")
	errExportRequestInvalid  = errors.New("marketdata: invalid export request")
)

// ExportRequest is one admitted export instruction.
type ExportRequest struct {
	Kind      ExportKind   `json:"kind"`
	Symbol    string       `json:"symbol"`
	Interval  string       `json:"interval,omitempty"` // klines only
	From      time.Time    `json:"from,omitempty"`     // inclusive; zero = unbounded
	To        time.Time    `json:"to,omitempty"`       // exclusive; zero = unbounded
	Format    ExportFormat `json:"format"`
	AccountID int64        `json:"account_id"`
	Limit     int          `json:"limit"` // 0 = default bound (see Bound)
	Async     bool         `json:"async"`
}

// Bound resolves the row ceiling for the request: an explicit limit wins;
// async defaults to the hard cap; sync defaults to the inline ceiling.
func (r ExportRequest) Bound() int {
	if r.Limit > 0 {
		return r.Limit
	}
	if r.Async {
		return ExportMaxRows
	}
	return ExportSyncRows
}

// NeedsAsync reports whether the request must run through the job queue.
func (r ExportRequest) NeedsAsync() bool {
	return r.Async || r.Bound() > ExportSyncRows
}

// Validate applies the admission guards. nil = admitted.
func (r ExportRequest) Validate() error {
	switch r.Kind {
	case KindTrades, KindTicks, KindKlines:
	default:
		return fmt.Errorf("%w: %q", ErrExportKindInvalid, r.Kind)
	}
	switch r.Format {
	case FormatCSV, FormatJSON, FormatParquet:
	default:
		return fmt.Errorf("%w: %q", ErrExportFormatInvalid, r.Format)
	}
	if r.Kind == KindKlines && strings.TrimSpace(r.Interval) == "" {
		return fmt.Errorf("%w: interval required", ErrExportIntervalInvalid)
	}
	if strings.TrimSpace(r.Symbol) == "" {
		return fmt.Errorf("%w: symbol required", errExportRequestInvalid)
	}
	if !r.From.IsZero() && !r.To.IsZero() && !r.From.Before(r.To) {
		return fmt.Errorf("%w: from must precede to", errExportRequestInvalid)
	}
	if r.Limit < 0 || r.Limit > ExportMaxRows {
		return fmt.Errorf("%w: limit %d", ErrExportLimitExceeded, r.Limit)
	}
	return nil
}

// Row is one export row across kinds. Trade/tick kinds populate the tape
// fields; klines populate the OHLCV fields (Ts carries open_time).
type Row struct {
	TradeID  uint64
	Ts       time.Time
	Symbol   string
	Side     string
	Price    decimal.Decimal
	Quantity decimal.Decimal
	// Klines only.
	Interval    string
	Open        decimal.Decimal
	High        decimal.Decimal
	Low         decimal.Decimal
	Close       decimal.Decimal
	Volume      decimal.Decimal
	QuoteVolume decimal.Decimal
	TradeCount  int64
}

// Job is one export_jobs row (migration 256).
type Job struct {
	ID         int64
	AccountID  int64
	Kind       string
	Symbol     string
	Interval   string
	Format     string
	From       time.Time // zero = unbounded
	To         time.Time
	RowLimit   int
	Status     string
	ObjectRef  string
	RowCount   int64
	SHA256     string
	Truncated  bool
	Error      string
	ExpiresAt  *time.Time
	NotifiedAt *time.Time
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// Request reconstructs the ExportRequest the worker streams.
func (j Job) Request() ExportRequest {
	return ExportRequest{
		Kind: ExportKind(j.Kind), Symbol: j.Symbol, Interval: j.Interval,
		From: j.From, To: j.To, Format: ExportFormat(j.Format),
		AccountID: j.AccountID, Limit: j.RowLimit, Async: true,
	}
}

// Completion is the COMPLETED stamp payload.
type Completion struct {
	ObjectRef string
	RowCount  int64
	SHA256    string
	Truncated bool
	ExpiresAt time.Time
	At        time.Time
}

// JobStore is the export_jobs persistence seam — *PgJobStore in
// production, MemJobStore in unit tests.
type JobStore interface {
	// Insert enqueues one PENDING row and returns its id.
	Insert(ctx context.Context, j Job) (int64, error)
	// Get returns the row or (nil, nil) on miss.
	Get(ctx context.Context, id int64) (*Job, error)
	// ListForAccount pages one account's jobs newest-first; the
	// (created_at, id) keyset continues strictly below `after`
	// (zero after = first page).
	ListForAccount(ctx context.Context, accountID int64,
		afterCreatedAt time.Time, afterID int64, limit int) ([]Job, error)
	// ClaimPending atomically marks up to limit PENDING rows RUNNING
	// (FOR UPDATE SKIP LOCKED on PG) and returns them in FIFO order.
	ClaimPending(ctx context.Context, limit int) ([]Job, error)
	// Complete stamps a RUNNING row COMPLETED.
	Complete(ctx context.Context, id int64, c Completion) error
	// Fail stamps a RUNNING row FAILED with the error text.
	Fail(ctx context.Context, id int64, errMsg string, at time.Time) error
	// MarkNotified stamps notified_at on a COMPLETED row.
	MarkNotified(ctx context.Context, id int64, at time.Time) error
	// Unnotified returns COMPLETED rows missing notified_at — the link
	// email still owes the account a send.
	Unnotified(ctx context.Context, limit int) ([]Job, error)
}

// ObjectStore is the narrow artifact seam — objectstore.Client satisfies
// it; unit tests inject an in-memory fake.
type ObjectStore interface {
	Put(ctx context.Context, in objectstore.PutInput) (objectstore.Object, error)
	Get(ctx context.Context, key string) ([]byte, objectstore.Object, error)
	Delete(ctx context.Context, key string) error
}

// RecipientSource resolves the account's email address — the Task-20.3.8
// reporting.RecipientSource shape (PgRecipientSource satisfies it).
type RecipientSource interface {
	Email(ctx context.Context, accountID int64) (string, error)
}

// Notifier delivers the export-ready link. Production binds
// ReportingNotifier over the reporting.EmailSender SMTP/SES seam; dev
// uses LogNotifier; tests use a recording fake.
type Notifier interface {
	NotifyExportReady(ctx context.Context, to string, j Job, downloadURL string) error
}

// ReportingNotifier adapts the reporting.EmailSender seam (Task 20.3.8
// item 2b) — the link rides the email body, no attachment (the artifact
// stays in S3 behind the expiring URL, never in a mailbox).
type ReportingNotifier struct {
	Mail reporting.EmailSender
	// LinkBase is the public base URL prepended to the download path
	// (e.g. "https://app.exchange.example"); empty emits the API path.
	LinkBase string
}

// NotifyExportReady implements Notifier.
func (n ReportingNotifier) NotifyExportReady(ctx context.Context, to string, j Job, downloadURL string) error {
	if n.Mail == nil {
		return fmt.Errorf("marketdata: export notifier has no email sender")
	}
	subject := fmt.Sprintf("Your %s export is ready — job %d", j.Format, j.ID)
	expires := ""
	if j.ExpiresAt != nil {
		expires = j.ExpiresAt.UTC().Format(time.RFC3339)
	}
	body := fmt.Sprintf(
		"Your %s export of %s %s (job %d) is ready.\n\nDownload: %s\n\n"+
			"The link expires %s (24h after completion). Rows exported: %d.",
		j.Format, j.Symbol, j.Kind, j.ID, downloadURL, expires, j.RowCount)
	return n.Mail.SendEmail(ctx, reporting.Email{
		To: to, Subject: subject, Body: body,
	})
}

// LogNotifier is the dev default — logs the link (recipient masked).
type LogNotifier struct{ Log *slog.Logger }

// NotifyExportReady implements Notifier.
func (l LogNotifier) NotifyExportReady(_ context.Context, to string, j Job, downloadURL string) error {
	lg := l.Log
	if lg == nil {
		lg = slog.Default()
	}
	at := to
	if i := strings.LastIndex(to, "@"); i > 0 {
		at = "***@" + to[i+1:]
	}
	lg.Info("export ready", "to", at, "job_id", j.ID, "url", downloadURL,
		"rows", j.RowCount)
	return nil
}

// ---------------------------------------------------------------------------
// Source seams — one per kind.
//
// marketdata cannot import internal/analytics (analytics → funding →
// marketdata is a cycle), so the seams below are defined over
// marketdata-owned row types. The composition layer (internal/api)
// binds the Phase-20 analytics stores through adapters
// (ExportTickSource / ExportKlineSource in handlers_export.go); the
// trades table has no analytics reader, so CHTradesStore lives here
// over the local CHQueryConn shape (analytics.Conn satisfies it).
// ---------------------------------------------------------------------------

// TapeQuery is one keyset page read of a tape table (trades or ticks) —
// newest-first on (ts, trade_id), mirroring the history endpoints.
type TapeQuery struct {
	Symbol string
	From   time.Time
	To     time.Time
	After  *TapeCursor
	Limit  int
}

// TapeCursor is the (ts, trade_id) keyset for the DESC tape stream.
type TapeCursor struct {
	Ts      time.Time
	TradeID uint64
}

// TradesRow is the public-tape projection of one trades/ticks row —
// account-linkage columns are deliberately absent.
type TradesRow struct {
	TradeID uint64
	Ts      time.Time
	Symbol  string
	Side    string
	Price   decimal.Decimal
	Qty     decimal.Decimal
}

// TapeQuerier is the tape-table read seam — *CHTradesStore (trades) or
// the api-layer tick adapter (ticks) satisfy it.
type TapeQuerier interface {
	QueryTape(ctx context.Context, q TapeQuery) ([]TradesRow, error)
}

// KlineQuery is one keyset page read of a per-interval OHLCV table —
// oldest-first on open_time.
type KlineQuery struct {
	Symbol   string
	Interval string
	From     time.Time
	To       time.Time
	After    *time.Time
	Limit    int
}

// KlineRow is one closed candle for export.
type KlineRow struct {
	OpenTime    time.Time
	Symbol      string
	Interval    string
	Open        decimal.Decimal
	High        decimal.Decimal
	Low         decimal.Decimal
	Close       decimal.Decimal
	Volume      decimal.Decimal
	QuoteVolume decimal.Decimal
	TradeCount  int64
}

// KlineQuerier is the candle read seam — the api-layer adapter over
// *analytics.OHLCVStore satisfies it.
type KlineQuerier interface {
	QueryKlines(ctx context.Context, q KlineQuery) ([]KlineRow, error)
}

// CHQueryConn is the read half of analytics.Conn — declared locally so
// this package never imports analytics (import cycle); any
// analytics.Conn satisfies it structurally.
type CHQueryConn interface {
	Query(ctx context.Context, query string, args ...any) (driver.Rows, error)
}

// CHTradesStore reads the ClickHouse `trades` table (schema
// 002_trades.sql — ReplacingMergeTree(ver), PARTITION BY toYYYYMM(ts)).
// FINAL collapses unmerged re-ingests, matching the TickStore read
// contract in analytics.
type CHTradesStore struct {
	conn CHQueryConn
}

// NewCHTradesStore wires the store over an open analytics.Conn.
func NewCHTradesStore(conn CHQueryConn) *CHTradesStore {
	return &CHTradesStore{conn: conn}
}

// exportTradesReadColumns is the export projection — tape fields only;
// the maker/taker account columns never leave ClickHouse.
const exportTradesReadColumns = "ts, trade_id, symbol, aggressor_side, price, qty"

// QueryTape implements TapeQuerier.
func (s *CHTradesStore) QueryTape(ctx context.Context, q TapeQuery) ([]TradesRow, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	var b strings.Builder
	b.WriteString("SELECT " + exportTradesReadColumns +
		" FROM trades FINAL WHERE symbol = ?")
	args := []any{q.Symbol}
	if !q.From.IsZero() {
		b.WriteString(" AND ts >= ?")
		args = append(args, q.From.UTC())
	}
	if !q.To.IsZero() {
		b.WriteString(" AND ts < ?")
		args = append(args, q.To.UTC())
	}
	if q.After != nil {
		b.WriteString(" AND (ts, trade_id) < (?, ?)")
		args = append(args, q.After.Ts.UTC(), q.After.TradeID)
	}
	b.WriteString(" ORDER BY ts DESC, trade_id DESC LIMIT ?")
	args = append(args, limit)

	rows, err := s.conn.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("trades export query %s: %w", q.Symbol, err)
	}
	defer rows.Close()
	out := make([]TradesRow, 0, limit)
	for rows.Next() {
		var r TradesRow
		if err := rows.Scan(&r.Ts, &r.TradeID, &r.Symbol, &r.Side,
			&r.Price, &r.Qty); err != nil {
			return nil, fmt.Errorf("trades export row scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trades export rows: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// ExportService — admission, render, worker.
// ---------------------------------------------------------------------------

// ExportService owns the export lifecycle. Source seams map the kind to
// its CH projection; Jobs/Objects/Notifier/Recipients drive the async
// path. All fields are optional at construction — a nil seam degrades the
// paths that need it to ErrExportSourceMissing (SERVICE_DEGRADED at the
// edge), never a panic.
type ExportService struct {
	Trades  TapeQuerier  // trades table read seam
	Ticks   TapeQuerier  // ticks table read seam
	Klines  KlineQuerier // OHLCV read seam
	Jobs    JobStore
	Objects ObjectStore

	// IntervalOK validates a kline interval against the persisted
	// timeframe set — the orchestrator binds analytics.PersistedInterval.
	// Required when the klines kind is wired.
	IntervalOK func(string) bool

	Notifier   Notifier        // nil → link email skipped
	Recipients RecipientSource // required when Notifier != nil
	// LinkBase is the public base URL for emailed download links; the
	// email carries LinkBase + /api/v1/export-jobs/{id}/download.
	LinkBase string

	Now  func() time.Time
	Logf func(string, ...any)
}

func (s *ExportService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Clock is the nil-safe public accessor for the service clock — the API
// layer uses it to stamp wire docs consistently with job timestamps.
func (s *ExportService) Clock() time.Time { return s.now() }

func (s *ExportService) log(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Admit runs the admission guards and checks the kind's source seam is
// wired. Shared by the sync and async entry points.
func (s *ExportService) Admit(req ExportRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	switch req.Kind {
	case KindTrades:
		if s.Trades == nil {
			return fmt.Errorf("%w: trades", ErrExportSourceMissing)
		}
	case KindTicks:
		if s.Ticks == nil {
			return fmt.Errorf("%w: ticks", ErrExportSourceMissing)
		}
	case KindKlines:
		if s.Klines == nil {
			return fmt.Errorf("%w: klines", ErrExportSourceMissing)
		}
		if s.IntervalOK == nil {
			return fmt.Errorf("%w: kline interval validator", ErrExportSourceMissing)
		}
		if !s.IntervalOK(req.Interval) {
			return fmt.Errorf("%w: %q", ErrExportIntervalInvalid, req.Interval)
		}
	}
	return nil
}

// pageCursor is the internal keyset position — (ts, trade_id) for
// trades/ticks (DESC), (bucket, 0) for klines (ASC).
type pageCursor struct {
	Ts time.Time
	ID uint64
}

// page reads up to n rows continuing after cur (cur.Ts zero = first
// page). The returned cursor is the last emitted row's position.
func (s *ExportService) page(ctx context.Context, req ExportRequest,
	cur pageCursor, n int) ([]Row, pageCursor, error) {
	switch req.Kind {
	case KindTrades:
		tq := TapeQuery{Symbol: req.Symbol, From: req.From, To: req.To, Limit: n}
		if !cur.Ts.IsZero() {
			tq.After = &TapeCursor{Ts: cur.Ts, TradeID: cur.ID}
		}
		rows, err := s.Trades.QueryTape(ctx, tq)
		if err != nil {
			return nil, cur, err
		}
		out := make([]Row, len(rows))
		for i, t := range rows {
			out[i] = Row{TradeID: t.TradeID, Ts: t.Ts.UTC(), Symbol: t.Symbol,
				Side: t.Side, Price: t.Price, Quantity: t.Qty}
		}
		if len(rows) > 0 {
			last := rows[len(rows)-1]
			cur = pageCursor{Ts: last.Ts.UTC(), ID: last.TradeID}
		}
		return out, cur, nil
	case KindTicks:
		tq := TapeQuery{Symbol: req.Symbol, From: req.From, To: req.To, Limit: n}
		if !cur.Ts.IsZero() {
			tq.After = &TapeCursor{Ts: cur.Ts, TradeID: cur.ID}
		}
		rows, err := s.Ticks.QueryTape(ctx, tq)
		if err != nil {
			return nil, cur, err
		}
		out := make([]Row, len(rows))
		for i, t := range rows {
			out[i] = Row{TradeID: t.TradeID, Ts: t.Ts.UTC(), Symbol: t.Symbol,
				Side: t.Side, Price: t.Price, Quantity: t.Qty}
		}
		if len(rows) > 0 {
			last := rows[len(rows)-1]
			cur = pageCursor{Ts: last.Ts.UTC(), ID: last.TradeID}
		}
		return out, cur, nil
	case KindKlines:
		kq := KlineQuery{Symbol: req.Symbol, Interval: req.Interval,
			From: req.From, To: req.To, Limit: n}
		if !cur.Ts.IsZero() {
			after := cur.Ts
			kq.After = &after
		}
		rows, err := s.Klines.QueryKlines(ctx, kq)
		if err != nil {
			return nil, cur, err
		}
		out := make([]Row, len(rows))
		for i, b := range rows {
			out[i] = Row{
				Ts: b.OpenTime.UTC(), Symbol: b.Symbol, Interval: b.Interval,
				Open: b.Open, High: b.High, Low: b.Low, Close: b.Close,
				Volume: b.Volume, QuoteVolume: b.QuoteVolume,
				TradeCount: b.TradeCount,
			}
		}
		if len(rows) > 0 {
			cur = pageCursor{Ts: rows[len(rows)-1].OpenTime.UTC()}
		}
		return out, cur, nil
	}
	return nil, cur, fmt.Errorf("%w: %q", ErrExportKindInvalid, req.Kind)
}

// streamRows renders up to req.Bound() rows through the encoder in
// ExportPageRows keyset batches. truncated=true means the range still
// held rows past the bound (one extra row is probed, never emitted).
func (s *ExportService) streamRows(ctx context.Context, req ExportRequest,
	w io.Writer, async bool) (rows int64, truncated bool, err error) {
	bound := req.Bound()
	enc, err := newRowEncoder(req.Format, req.Kind, async, w)
	if err != nil {
		return 0, false, err
	}
	var cur pageCursor
	for rows < int64(bound) {
		n := int64(bound) - rows
		if n > ExportPageRows {
			n = ExportPageRows
		}
		page, next, perr := s.page(ctx, req, cur, int(n))
		if perr != nil {
			return rows, false, perr
		}
		cur = next
		for i := range page {
			if err := enc.Write(page[i]); err != nil {
				return rows, false, err
			}
			rows++
		}
		if len(page) < int(n) {
			break // source exhausted
		}
	}
	if rows == int64(bound) {
		more, _, perr := s.page(ctx, req, cur, 1)
		if perr != nil {
			return rows, false, perr
		}
		truncated = len(more) > 0
	}
	if err := enc.Flush(); err != nil {
		return rows, truncated, err
	}
	return rows, truncated, nil
}

// Stream renders a synchronous export to w. Callers (the REST handler)
// set response headers before invoking; ctx carries the request
// cancellation.
func (s *ExportService) Stream(ctx context.Context, req ExportRequest,
	w io.Writer) (int64, bool, error) {
	return s.streamRows(ctx, req, w, false)
}

// Enqueue writes one PENDING export_jobs row for the async path.
func (s *ExportService) Enqueue(ctx context.Context, req ExportRequest) (*Job, error) {
	if s.Jobs == nil {
		return nil, fmt.Errorf("%w: job store", ErrExportSourceMissing)
	}
	j := Job{
		AccountID: req.AccountID, Kind: string(req.Kind), Symbol: req.Symbol,
		Interval: req.Interval, Format: string(req.Format),
		From: req.From, To: req.To, RowLimit: req.Bound(),
		Status: JobPending, CreatedAt: s.now(),
	}
	id, err := s.Jobs.Insert(ctx, j)
	if err != nil {
		return nil, err
	}
	j.ID = id
	return &j, nil
}

// DownloadURL renders the link the email/status doc carries.
func (s *ExportService) DownloadURL(j Job) string {
	base := strings.TrimRight(s.LinkBase, "/")
	return fmt.Sprintf("%s/api/v1/export-jobs/%d/download", base, j.ID)
}

// objectKey is the artifact's S3 key — account- and job-scoped so a
// re-run never collides with a sibling export.
func (s *ExportService) objectKey(j Job) string {
	ext := map[ExportFormat]string{
		FormatCSV: "csv", FormatJSON: "jsonl", FormatParquet: "parquet",
	}[ExportFormat(j.Format)]
	if ext == "" {
		ext = "bin"
	}
	return fmt.Sprintf("exports/%d/%d.%s", j.AccountID, j.ID, ext)
}

// Filename is the Content-Disposition filename for downloads/sync.
func (s *ExportService) Filename(j Job) string {
	ext := map[ExportFormat]string{
		FormatCSV: "csv", FormatJSON: "json", FormatParquet: "parquet",
	}[ExportFormat(j.Format)]
	if ext == "" {
		ext = "bin"
	}
	sym := strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			return r
		default:
			return '-'
		}
	}, j.Symbol)
	return fmt.Sprintf("%s_%s_%d.%s", j.Kind, sym, j.ID, ext)
}

// ContentType resolves the artifact media type. async distinguishes the
// sync JSON envelope (application/json) from the async JSONL file
// (application/x-ndjson).
func ContentType(format string, async bool) string {
	switch ExportFormat(format) {
	case FormatCSV:
		return "text/csv"
	case FormatJSON:
		if async {
			return "application/x-ndjson"
		}
		return "application/json"
	case FormatParquet:
		return "application/vnd.apache.parquet"
	}
	return "application/octet-stream"
}

// ProcessJob renders one claimed (RUNNING) job into the object store,
// verifies the write (ETag md5 + recorded sha256), stamps it COMPLETED
// with the 24h expiry, and dispatches the link email. Any failure marks
// the row FAILED with the error text — the artifact of record is the
// job row, and a FAILED job is the honest terminal state (§2.7).
func (s *ExportService) ProcessJob(ctx context.Context, j Job) error {
	if s.Objects == nil {
		return s.failJob(ctx, j, "object store not configured")
	}
	key := s.objectKey(j)
	sha := sha256.New()
	md := md5.New()
	pr, pw := io.Pipe()
	type result struct {
		rows      int64
		truncated bool
		err       error
	}
	resCh := make(chan result, 1)
	go func() {
		var r result
		r.rows, r.truncated, r.err = s.streamRows(ctx, j.Request(),
			io.MultiWriter(pw, sha, md), true)
		_ = pw.CloseWithError(r.err)
		resCh <- r
	}()
	obj, putErr := s.Objects.Put(ctx, objectstore.PutInput{
		Key:         key,
		Body:        pr,
		Size:        -1, // unknown — the client buffers unknown-size puts
		ContentType: ContentType(j.Format, true),
		Metadata: map[string]string{
			"account_id": fmt.Sprint(j.AccountID),
			"kind":       j.Kind, "symbol": j.Symbol, "format": j.Format,
		},
	})
	res := <-resCh
	if res.err != nil {
		return s.failJob(ctx, j, "render: "+res.err.Error())
	}
	if putErr != nil {
		return s.failJob(ctx, j, "object put: "+putErr.Error())
	}
	// Zero-loss guard: the seam contract returns hex(md5(body)) — a
	// mismatch means the stored bytes are not what we produced.
	if obj.ETag != "" && !strings.EqualFold(obj.ETag, hex.EncodeToString(md.Sum(nil))) {
		return s.failJob(ctx, j, "object etag mismatch — stored bytes unverified")
	}
	expires := s.now().Add(ExportLinkTTL)
	if err := s.Jobs.Complete(ctx, j.ID, Completion{
		ObjectRef: key, RowCount: res.rows,
		SHA256:    hex.EncodeToString(sha.Sum(nil)),
		Truncated: res.truncated, ExpiresAt: expires, At: s.now(),
	}); err != nil {
		return fmt.Errorf("marketdata: complete export job %d: %w", j.ID, err)
	}
	j.Status = JobCompleted
	j.ObjectRef = key
	j.RowCount = res.rows
	j.ExpiresAt = &expires
	j.Truncated = res.truncated
	s.notify(ctx, j)
	return nil
}

func (s *ExportService) failJob(ctx context.Context, j Job, msg string) error {
	if s.Jobs == nil {
		return fmt.Errorf("marketdata: export job %d failed (%s) and no job store", j.ID, msg)
	}
	if err := s.Jobs.Fail(ctx, j.ID, msg, s.now()); err != nil {
		return fmt.Errorf("marketdata: fail export job %d: %w", j.ID, err)
	}
	return fmt.Errorf("marketdata: export job %d: %s", j.ID, msg)
}

// notify dispatches the download-link email through the delivery seam
// and stamps notified_at. A notify failure is logged — the row stays
// COMPLETED with notified_at NULL so the next RunDue pass re-sends.
func (s *ExportService) notify(ctx context.Context, j Job) {
	if s.Notifier == nil || s.Recipients == nil {
		return
	}
	to, err := s.Recipients.Email(ctx, j.AccountID)
	if err != nil {
		s.log("marketdata: export job %d recipient lookup failed: %v", j.ID, err)
		return
	}
	if to == "" {
		return
	}
	if err := s.Notifier.NotifyExportReady(ctx, to, j, s.DownloadURL(j)); err != nil {
		s.log("marketdata: export job %d link email failed: %v", j.ID, err)
		return
	}
	if err := s.Jobs.MarkNotified(ctx, j.ID, s.now()); err != nil {
		s.log("marketdata: export job %d mark notified: %v", j.ID, err)
	}
}

// RunDue claims up to limit PENDING jobs and processes them, then
// re-sends link email for COMPLETED rows still missing notified_at.
// Returns the processed count.
func (s *ExportService) RunDue(ctx context.Context, limit int) (int, error) {
	if s.Jobs == nil {
		return 0, fmt.Errorf("%w: job store", ErrExportSourceMissing)
	}
	if limit <= 0 {
		limit = 10
	}
	jobs, err := s.Jobs.ClaimPending(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("marketdata: claim export jobs: %w", err)
	}
	for _, j := range jobs {
		if err := s.ProcessJob(ctx, j); err != nil {
			s.log("marketdata: %v", err)
		}
	}
	if s.Notifier != nil && s.Recipients != nil {
		un, uerr := s.Jobs.Unnotified(ctx, limit)
		if uerr != nil {
			s.log("marketdata: unnotified export scan: %v", uerr)
		}
		for _, j := range un {
			s.notify(ctx, j)
		}
	}
	return len(jobs), nil
}

// Run loops RunDue on the given cadence until ctx cancels — the worker
// entry point the orchestrator binds (mirrors DeliveryScheduler.Run).
func (s *ExportService) Run(ctx context.Context, cadence time.Duration) {
	if cadence <= 0 {
		cadence = 5 * time.Second
	}
	if _, err := s.RunDue(ctx, 0); err != nil {
		s.log("marketdata: export sweep: %v", err)
	}
	t := time.NewTicker(cadence)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.RunDue(ctx, 0); err != nil {
				s.log("marketdata: export sweep: %v", err)
			}
		}
	}
}

// Download is one rendered artifact fetched for the owning account.
type Download struct {
	Job         *Job
	Body        []byte
	ContentType string
	Filename    string
}

// DownloadForAccount fetches the artifact for a COMPLETED, unexpired job
// owned by accountID. Unknown/cross-account/expired jobs all surface
// ErrExportJobNotFound/ErrExportExpired — the endpoint must not confirm
// existence (fail-closed privacy).
func (s *ExportService) DownloadForAccount(ctx context.Context, id, accountID int64) (*Download, error) {
	if s.Jobs == nil || s.Objects == nil {
		return nil, fmt.Errorf("%w: download path", ErrExportSourceMissing)
	}
	j, err := s.Jobs.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if j == nil || j.AccountID != accountID {
		return nil, ErrExportJobNotFound
	}
	if j.Status != JobCompleted {
		return nil, ErrExportNotCompleted
	}
	if j.ExpiresAt != nil && !s.now().Before(*j.ExpiresAt) {
		return nil, ErrExportExpired
	}
	body, _, err := s.Objects.Get(ctx, j.ObjectRef)
	if err != nil {
		return nil, fmt.Errorf("marketdata: fetch export artifact %q: %w", j.ObjectRef, err)
	}
	return &Download{
		Job: j, Body: body,
		ContentType: ContentType(j.Format, true), Filename: s.Filename(*j),
	}, nil
}
