// Phase-23 Task 23.3.2 — data export REST surface (spec §10, §16.6).
//
//	GET /api/v1/history/trades/{symbol}/export?format=csv&from=&to=&limit=&async=&kind=
//	GET /api/v1/export-jobs                — own jobs, §8.8 envelope
//	GET /api/v1/export-jobs/{id}           — job status (owner-scoped)
//	GET /api/v1/export-jobs/{id}/download  — artifact fetch (owner, ≤24h)
//
// Admission contract (internal/marketdata.ExportService):
//
//   - format csv|json|parquet (absent defaults to csv; an unknown value
//     is EXPORT_FORMAT_INVALID — never silently defaulted);
//   - kind trades|ticks|klines via ?kind= (default trades, matching the
//     route); klines take ?interval= from the 12 persisted timeframes;
//   - limit ≤ 1,000,000 — beyond is EXPORT_LIMIT_EXCEEDED;
//   - ?async=1, or a bound over the 50k inline ceiling, enqueues an
//     export_jobs row and answers 202 {job}; otherwise the export
//     renders inline (bounded: rendered to memory first so a render
//     failure answers an error envelope rather than a truncated file);
//   - job status/list/download are owner-scoped — cross-account and
//     expired jobs are EXPORT_JOB_NOT_FOUND, never a confirmable miss.
package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"exchange/internal/analytics"
	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/marketdata"
)

// ExportDeps bundles the export surface seams.
type ExportDeps struct {
	Exporter    *marketdata.ExportService
	Instruments InstrumentResolver // same resolver as the history surface
}

// exportJobsSpec is the pagination contract for the jobs list — declared
// locally per the historyKlinesSpec precedent (the published ListSpecs
// table is owned by the Task 5.3.42 cluster).
var exportJobsSpec = &ListSpec{
	Path: "/api/v1/export-jobs", Default: 50, Max: 200,
	Sortable:   []string{"created_at", "id"},
	Filterable: []string{"status", "kind", "format"},
}

// exportJobDoc is the wire projection of one export_jobs row.
type exportJobDoc struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	Symbol      string `json:"symbol"`
	Interval    string `json:"interval,omitempty"`
	Format      string `json:"format"`
	From        string `json:"from,omitempty"`
	To          string `json:"to,omitempty"`
	RowLimit    int    `json:"row_limit"`
	Status      string `json:"status"`
	RowCount    int64  `json:"row_count"`
	Truncated   bool   `json:"truncated"`
	SHA256      string `json:"sha256,omitempty"`
	DownloadURL string `json:"download_url,omitempty"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	Error       string `json:"error,omitempty"`
	CreatedAt   string `json:"created_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
}

// jobDoc renders one job row for the wire. download_url is only
// populated for a COMPLETED job whose link has not expired — an expired
// link is a miss, never a stale ref.
func jobDoc(svc *marketdata.ExportService, j marketdata.Job, now time.Time) exportJobDoc {
	d := exportJobDoc{
		ID: j.ID, Kind: j.Kind, Symbol: j.Symbol, Interval: j.Interval,
		Format: j.Format, RowLimit: j.RowLimit, Status: j.Status,
		RowCount: j.RowCount, Truncated: j.Truncated, SHA256: j.SHA256,
		Error: j.Error, CreatedAt: j.CreatedAt.UTC().Format(time.RFC3339),
	}
	if !j.From.IsZero() {
		d.From = j.From.UTC().Format(time.RFC3339)
	}
	if !j.To.IsZero() {
		d.To = j.To.UTC().Format(time.RFC3339)
	}
	if j.Status == marketdata.JobCompleted && j.ExpiresAt != nil &&
		now.Before(*j.ExpiresAt) {
		d.DownloadURL = svc.DownloadURL(j)
	}
	if j.ExpiresAt != nil {
		d.ExpiresAt = j.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if j.FinishedAt != nil {
		d.FinishedAt = j.FinishedAt.UTC().Format(time.RFC3339)
	}
	return d
}

// exportErr maps marketdata's typed failures onto the §23 registry.
func exportErr(w http.ResponseWriter, r *http.Request, err error) {
	rid := gateway.RequestIDFrom(r.Context())
	switch {
	case errors.Is(err, marketdata.ErrExportFormatInvalid):
		WriteError(w, "EXPORT_FORMAT_INVALID", err.Error(), rid,
			map[string]any{"formats": []string{"csv", "json", "parquet"}})
	case errors.Is(err, marketdata.ErrExportLimitExceeded):
		WriteError(w, "EXPORT_LIMIT_EXCEEDED", err.Error(), rid,
			map[string]any{"max_rows": marketdata.ExportMaxRows})
	case errors.Is(err, marketdata.ErrExportJobNotFound),
		errors.Is(err, marketdata.ErrExportExpired):
		WriteError(w, "EXPORT_JOB_NOT_FOUND", err.Error(), rid, nil)
	case errors.Is(err, marketdata.ErrExportNotCompleted):
		WriteError(w, "INVALID_REQUEST", err.Error(), rid, nil)
	case errors.Is(err, marketdata.ErrExportIntervalInvalid):
		WriteError(w, "INVALID_REQUEST", err.Error(), rid, map[string]any{
			"intervals": analytics.PersistedIntervalLabels(),
		})
	case errors.Is(err, marketdata.ErrExportSourceMissing):
		WriteError(w, "SERVICE_DEGRADED",
			"export path not configured", rid, nil)
	default:
		WriteError(w, "INVALID_REQUEST", err.Error(), rid, nil)
	}
}

// exportAccount resolves the caller's account context. TierBasic routes
// always carry claims through middleware; the nil-guard keeps the
// handler honest when it is mounted bare in tests/dev.
func exportAccount(w http.ResponseWriter, r *http.Request) (int64, bool) {
	claims := auth.ClaimsFrom(r.Context())
	if claims == nil || claims.AccountID == 0 {
		WriteError(w, "UNAUTHORIZED", "authentication required",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	return claims.AccountID, true
}

// ---------------------------------------------------------------------------
// GET /api/v1/history/trades/{symbol}/export
// ---------------------------------------------------------------------------

// HistoryTradesExport admits and executes one market-data export. The
// sync path streams ≤50k rows inline; the async path enqueues the job
// the worker renders to S3 and emails the 24h download link for.
func HistoryTradesExport(d *ExportDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d == nil || d.Exporter == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"export service not configured",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		accountID, ok := exportAccount(w, r)
		if !ok {
			return
		}
		symbol := r.PathValue("symbol")
		if symbol == "" {
			WriteError(w, "INVALID_REQUEST", "symbol required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := r.URL.Query()

		format := marketdata.ExportFormat(strings.ToLower(q.Get("format")))
		if format == "" {
			format = marketdata.FormatCSV
		}
		kind := marketdata.ExportKind(strings.ToLower(q.Get("kind")))
		if kind == "" {
			kind = marketdata.KindTrades // route is the trades export
		}
		interval := q.Get("interval")
		if interval == "" {
			interval = "1m"
		}
		from, err := parseTimeQuery(q.Get("from"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "from must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		to, err := parseTimeQuery(q.Get("to"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "to must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var limit int
		if raw := q.Get("limit"); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil || v < 1 {
				WriteError(w, "INVALID_REQUEST",
					"limit must be a positive integer",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			limit = v
		}
		async := q.Get("async") == "1" || strings.EqualFold(q.Get("async"), "true")

		req := marketdata.ExportRequest{
			Kind: kind, Symbol: symbol, Interval: interval,
			Format: format, AccountID: accountID, Limit: limit, Async: async,
		}
		if from != nil {
			req.From = *from
		}
		if to != nil {
			req.To = *to
		}
		if err := d.Exporter.Admit(req); err != nil {
			exportErr(w, r, err)
			return
		}
		if !resolveInstrument(w, r, d.Instruments, symbol) {
			return
		}

		if req.NeedsAsync() {
			job, err := d.Exporter.Enqueue(r.Context(), req)
			if err != nil {
				exportErr(w, r, err)
				return
			}
			WriteJSON(w, http.StatusAccepted, map[string]any{
				"job":        jobDoc(d.Exporter, *job, d.Exporter.Clock()),
				"status_url": fmt.Sprintf("/api/v1/export-jobs/%d", job.ID),
			})
			return
		}

		// Sync path: render into a bounded buffer first — a render
		// failure must answer an error envelope, never a truncated
		// download (fail-closed, spec §2.7). The 50k-row inline ceiling
		// bounds the buffer (~4MB worst case CSV).
		var buf bytes.Buffer
		rows, truncated, err := d.Exporter.Stream(r.Context(), req, &buf)
		if err != nil {
			exportErr(w, r, err)
			return
		}
		if truncated {
			w.Header().Set("X-Export-Truncated", "true")
		}
		w.Header().Set("X-Export-Rows", strconv.FormatInt(rows, 10))
		w.Header().Set("Content-Type", marketdata.ContentType(string(format), false))
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`attachment; filename="%s"`,
				d.Exporter.Filename(marketdata.Job{
					Kind: string(kind), Symbol: symbol, Format: string(format)})))
		w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf.Bytes())
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/export-jobs/{id}
// ---------------------------------------------------------------------------

// ExportJobStatus serves one job's lifecycle view — owner-scoped;
// cross-account ids resolve as misses (EXPORT_JOB_NOT_FOUND), never as
// 403 confirmations (§2.7 privacy pessimism).
func ExportJobStatus(d *ExportDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d == nil || d.Exporter == nil || d.Exporter.Jobs == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"export jobs not configured",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		accountID, ok := exportAccount(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "EXPORT_JOB_NOT_FOUND", "export job not found",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		j, err := d.Exporter.Jobs.Get(r.Context(), id)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED", "export job store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if j == nil || j.AccountID != accountID {
			WriteError(w, "EXPORT_JOB_NOT_FOUND", "export job not found",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, jobDoc(d.Exporter, *j, d.Exporter.Clock()))
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/export-jobs
// ---------------------------------------------------------------------------

// ExportJobList pages the caller's own jobs newest-first under the §8.8
// envelope ({data, next_cursor, limit, total}).
func ExportJobList(d *ExportDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d == nil || d.Exporter == nil || d.Exporter.Jobs == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"export jobs not configured",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		accountID, ok := exportAccount(w, r)
		if !ok {
			return
		}
		p, err := ParseListParams(r, exportJobsSpec)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var afterCreatedAt time.Time
		var afterID int64
		if p.Decoded != nil {
			afterCreatedAt = p.Decoded.CreatedAt
			afterID = p.Decoded.ID
		}
		jobs, err := d.Exporter.Jobs.ListForAccount(r.Context(), accountID,
			afterCreatedAt, afterID, p.Limit)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED", "export job store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		now := d.Exporter.Clock()
		docs := make([]exportJobDoc, 0, len(jobs))
		for _, j := range jobs {
			docs = append(docs, jobDoc(d.Exporter, j, now))
		}
		env := NewListEnvelope(docs, p,
			PageCursors(jobs, func(j marketdata.Job) (time.Time, int64) {
				return j.CreatedAt, j.ID
			}), int64(len(jobs)))
		WriteJSON(w, http.StatusOK, env)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/export-jobs/{id}/download
// ---------------------------------------------------------------------------

// ExportJobDownload streams the rendered artifact to the job owner while
// its 24h link is live. Cross-account, pending and expired ids are all
// EXPORT_JOB_NOT_FOUND.
func ExportJobDownload(d *ExportDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d == nil || d.Exporter == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"export service not configured",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		accountID, ok := exportAccount(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "EXPORT_JOB_NOT_FOUND", "export job not found",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		dl, err := d.Exporter.DownloadForAccount(r.Context(), id, accountID)
		if err != nil {
			exportErr(w, r, err)
			return
		}
		w.Header().Set("Content-Type", dl.ContentType)
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`attachment; filename="%s"`, dl.Filename))
		w.Header().Set("Content-Length", strconv.Itoa(len(dl.Body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(dl.Body)
	}
}

// ---------------------------------------------------------------------------
// Analytics source adapters — marketdata cannot import internal/analytics
// (analytics → funding → marketdata is a cycle), so the binding between
// the Phase-20 read projections and the export seams lives here at the
// composition layer.
// ---------------------------------------------------------------------------

// ExportTickSource adapts *analytics.TickStore to marketdata.TapeQuerier
// — the export reads the same ticks-projection contract as the history
// endpoint (keyset (ts,trade_id) DESC, FINAL dedup).
type ExportTickSource struct {
	Store *analytics.TickStore
}

// QueryTape implements marketdata.TapeQuerier.
func (a ExportTickSource) QueryTape(ctx context.Context, q marketdata.TapeQuery) ([]marketdata.TradesRow, error) {
	if a.Store == nil {
		return nil, marketdata.ErrExportSourceMissing
	}
	tq := analytics.TickQuery{
		Symbol: q.Symbol, From: q.From, To: q.To, Limit: q.Limit,
	}
	if q.After != nil {
		tq.After = &analytics.TickCursor{Ts: q.After.Ts, TradeID: q.After.TradeID}
	}
	ticks, err := a.Store.Query(ctx, tq)
	if err != nil {
		return nil, err
	}
	out := make([]marketdata.TradesRow, len(ticks))
	for i, t := range ticks {
		out[i] = marketdata.TradesRow{
			TradeID: t.TradeID, Ts: t.Ts, Symbol: t.Symbol,
			Side: t.Side, Price: t.Price, Qty: t.Qty,
		}
	}
	return out, nil
}

// ExportKlineSource adapts *analytics.OHLCVStore to
// marketdata.KlineQuerier — same persisted-interval tables as the
// history klines endpoint (bucket-ASC keyset).
type ExportKlineSource struct {
	Store *analytics.OHLCVStore
}

// QueryKlines implements marketdata.KlineQuerier.
func (a ExportKlineSource) QueryKlines(ctx context.Context, q marketdata.KlineQuery) ([]marketdata.KlineRow, error) {
	if a.Store == nil {
		return nil, marketdata.ErrExportSourceMissing
	}
	kq := analytics.OHLCVQuery{
		Symbol: q.Symbol, Interval: q.Interval,
		From: q.From, To: q.To, After: q.After, Limit: q.Limit,
	}
	bars, err := a.Store.Query(ctx, kq)
	if err != nil {
		return nil, err
	}
	out := make([]marketdata.KlineRow, len(bars))
	for i, b := range bars {
		out[i] = marketdata.KlineRow{
			OpenTime: b.OpenTime, Symbol: b.Symbol, Interval: b.Interval,
			Open: b.Open, High: b.High, Low: b.Low, Close: b.Close,
			Volume: b.Volume, QuoteVolume: b.QuoteVolume,
			TradeCount: b.TradeCount,
		}
	}
	return out, nil
}
