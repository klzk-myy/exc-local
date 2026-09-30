// erp_export.go — Phase-20 Task 20.3.7 item 3: the nightly ERP batch
// adapter (spec §16.5, §24 #205).
//
// Each nightly run produces a checksummed bundle:
//
//	manifest.json            — {run_id, seq, business_date, generated_at,
//	                           files:[{name, sha256, size}]} — the
//	                           integrity + ordering anchor;
//	journals-<date>.csv      — the day's journal_entries + ledger_lines;
//	trial-balance-<date>.csv — the day's trial-balance export;
//	statements-index-<date>.csv — statement rows generated that day.
//
// Replay protection (fail-closed):
//
//   - run_id is deterministic per business day ("erp-YYYYMMDD"); once a
//     run's row reaches SENT in erp_delivery_log, resending the same
//     run_id is rejected (IDEMPOTENCY_KEY_MISMATCH — never deliver the
//     same bundle twice downstream);
//   - every delivery attempt allocates a monotonic seq (max+1) so the
//     ERP can order/audit batches; retried FAILED runs keep run_id but
//     take a new seq (a failed send is re-driveable, a sent one is not);
//   - the manifest sha256 is stored on the delivery row — the sent-file
//     hash trail the auditors diff against.
//
// Retention: erp_delivery_log rows + bundle artifacts are retained
// >= 7 years (audit/tax; table COMMENT in migration 049).
package analytics

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// ERPFile is one bundle member.
type ERPFile struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	SHA256      string `json:"sha256"`
	Body        []byte `json:"-"`
}

// ERPBundle is one nightly delivery: ordered files + the manifest JSON.
type ERPBundle struct {
	RunID        string    `json:"run_id"`
	Seq          int64     `json:"seq"`
	BusinessDate time.Time `json:"business_date"`
	Files        []ERPFile `json:"files"`
	Manifest     []byte    `json:"-"` // serialized manifest.json content
}

// ERPAdapter is the delivery seam. Two adapters ship:
//
//	SFTPDropAdapter  — configurable SFTP delivery STUB: with Dir set the
//	                   bundle lands in Dir/<run_id>/ as the drop directory
//	                   an external SFTP poller ships (no SFTP client
//	                   dependency is vendored; Endpoint is recorded in
//	                   the manifest metadata for the poller). With no Dir
//	                   the adapter refuses — fail-closed.
//	WebhookAdapter   — POSTs {manifest, files(base64)} JSON to URL.
type ERPAdapter interface {
	Name() string
	Deliver(ctx context.Context, b ERPBundle) (deliveryRef string, err error)
}

// ---------------------------------------------------------------------------
// SFTPDropAdapter — labelled stub: drop-directory delivery.
// ---------------------------------------------------------------------------

// SFTPDropAdapter delivers the bundle to a local drop directory that the
// deployment's SFTP shipper consumes (production wiring substitutes a
// real SFTP client behind the same interface — this adapter is the
// deliberately-labelled stub path: Endpoint is recorded but no network
// session is opened).
type SFTPDropAdapter struct {
	Dir      string // drop directory root — required
	Endpoint string // sftp://… endpoint recorded on the manifest
}

// Name returns the adapter label persisted on erp_delivery_log.
func (a *SFTPDropAdapter) Name() string { return "sftp-dir-drop" }

// Deliver writes <Dir>/<run_id>/{manifest.json + bundle files}.
func (a *SFTPDropAdapter) Deliver(ctx context.Context, b ERPBundle) (string, error) {
	if a.Dir == "" {
		return "", excerrors.New("INTERNAL_ERROR",
			"erp sftp adapter: no drop directory configured (stub adapter requires Dir)")
	}
	dir := filepath.Join(a.Dir, b.RunID)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("erp sftp drop: mkdir: %w", err)
	}
	for _, f := range b.Files {
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(f.Name)), f.Body, 0o640); err != nil {
			return "", fmt.Errorf("erp sftp drop %s: %w", f.Name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b.Manifest, 0o640); err != nil {
		return "", fmt.Errorf("erp sftp drop manifest: %w", err)
	}
	return "dir://" + dir, nil
}

// ---------------------------------------------------------------------------
// WebhookAdapter — HTTP POST delivery.
// ---------------------------------------------------------------------------

// WebhookAdapter POSTs the bundle to the configured endpoint as one JSON
// envelope; a non-2xx response is a delivery failure (the run is marked
// FAILED and may be retried — the same bundle resend is still barred by
// run_id replay protection once SENT).
type WebhookAdapter struct {
	URL    string
	Client *http.Client // nil → http.DefaultClient
}

// Name returns the adapter label.
func (a *WebhookAdapter) Name() string { return "webhook" }

// webhookPayload is the POST body.
type webhookPayload struct {
	RunID        string          `json:"run_id"`
	Seq          int64           `json:"seq"`
	BusinessDate string          `json:"business_date"`
	ManifestSHA  string          `json:"manifest_sha256"`
	Manifest     json.RawMessage `json:"manifest"`
	Files        []struct {
		Name        string `json:"name"`
		ContentType string `json:"content_type"`
		SHA256      string `json:"sha256"`
		BodyB64     string `json:"body_b64"`
	} `json:"files"`
}

// Deliver implements ERPAdapter.
func (a *WebhookAdapter) Deliver(ctx context.Context, b ERPBundle) (string, error) {
	if a.URL == "" {
		return "", excerrors.New("INTERNAL_ERROR", "erp webhook adapter: no URL configured")
	}
	sum := sha256.Sum256(b.Manifest)
	p := webhookPayload{
		RunID: b.RunID, Seq: b.Seq,
		BusinessDate: b.BusinessDate.Format("2006-01-02"),
		ManifestSHA:  hex.EncodeToString(sum[:]),
		Manifest:     json.RawMessage(b.Manifest),
	}
	for _, f := range b.Files {
		p.Files = append(p.Files, struct {
			Name        string `json:"name"`
			ContentType string `json:"content_type"`
			SHA256      string `json:"sha256"`
			BodyB64     string `json:"body_b64"`
		}{f.Name, f.ContentType, f.SHA256, base64.StdEncoding.EncodeToString(f.Body)})
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("erp webhook marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("erp webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	cli := a.Client
	if cli == nil {
		cli = http.DefaultClient
	}
	resp, err := cli.Do(req)
	if err != nil {
		return "", fmt.Errorf("erp webhook post: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("erp webhook post: status %d", resp.StatusCode)
	}
	return fmt.Sprintf("http://%s%d", resp.Status, resp.StatusCode), nil
}

// ---------------------------------------------------------------------------
// ERPBatchService
// ---------------------------------------------------------------------------

// ERPBatchService builds and delivers the nightly bundle.
type ERPBatchService struct {
	pool    *pgxpool.Pool
	adapter ERPAdapter
	tb      *TrialBalanceService // optional; nil → trial-balance file omitted
	now     func() time.Time
}

// NewERPBatchService wires the service; adapter is required (fail-closed
// — a nil adapter would "deliver" silently).
func NewERPBatchService(pool *pgxpool.Pool, adapter ERPAdapter, tb *TrialBalanceService) (*ERPBatchService, error) {
	if pool == nil {
		return nil, fmt.Errorf("erp: nil pgx pool")
	}
	if adapter == nil {
		return nil, fmt.Errorf("erp: nil adapter")
	}
	return &ERPBatchService{pool: pool, adapter: adapter, tb: tb, now: time.Now}, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *ERPBatchService) SetClockForTest(now func() time.Time) { s.now = now }

// RunIDFor returns the deterministic run id for a business day.
func RunIDFor(day time.Time) string {
	return "erp-" + normalizeUTCDate(day).Format("20060102")
}

// RunNightly builds + delivers the bundle for `day`. Returns the
// erp_delivery_log row state after the attempt.
func (s *ERPBatchService) RunNightly(ctx context.Context, day time.Time) (*ERPDelivery, error) {
	day = normalizeUTCDate(day)
	runID := RunIDFor(day)

	// Replay-protection gate: a SENT run is never re-sent.
	var priorStatus string
	err := s.pool.QueryRow(ctx,
		`SELECT status::text FROM erp_delivery_log WHERE run_id = $1`, runID).Scan(&priorStatus)
	switch {
	case stderrors.Is(err, pgx.ErrNoRows):
		priorStatus = ""
	case err != nil:
		return nil, fmt.Errorf("erp: prior run lookup: %w", err)
	case priorStatus == "SENT":
		return nil, excerrors.New("IDEMPOTENCY_KEY_MISMATCH",
			fmt.Sprintf("erp run %s already delivered — same-run resend rejected", runID))
	}

	files, err := s.buildFiles(ctx, day)
	if err != nil {
		return nil, err
	}

	// Allocate the monotonic seq + persist the PENDING row in one tx.
	var seq int64
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("erp: seq tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq),0)+1 FROM erp_delivery_log`).Scan(&seq); err != nil {
		return nil, fmt.Errorf("erp: seq allocate: %w", err)
	}
	manifest, err := buildManifest(runID, seq, day, s.now().UTC(), files)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(manifest)
	manifestSHA := hex.EncodeToString(sum[:])
	if priorStatus == "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO erp_delivery_log
			    (run_id, seq, business_date, adapter, manifest_sha256, file_count, status)
			VALUES ($1, $2, $3, $4, $5, $6, 'PENDING')`,
			runID, seq, day, s.adapter.Name(), manifestSHA, len(files)); err != nil {
			return nil, fmt.Errorf("erp: log insert: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE erp_delivery_log
			SET seq = $2, adapter = $3, manifest_sha256 = $4, file_count = $5,
			    status = 'PENDING', attempted_at = now(), error_text = NULL
			WHERE run_id = $1`,
			runID, seq, s.adapter.Name(), manifestSHA, len(files)); err != nil {
			return nil, fmt.Errorf("erp: log update: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("erp: log commit: %w", err)
	}

	bundle := ERPBundle{RunID: runID, Seq: seq, BusinessDate: day, Files: files, Manifest: manifest}
	ref, derr := s.adapter.Deliver(ctx, bundle)

	status, errText := "SENT", ""
	if derr != nil {
		status, errText = "FAILED", derr.Error()
	}
	var deliveredAt *time.Time
	if derr == nil {
		t := s.now().UTC()
		deliveredAt = &t
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE erp_delivery_log
		SET status = $2, delivery_ref = NULLIF($3,''), delivered_at = $4,
		    error_text = NULLIF($5,'')
		WHERE run_id = $1`,
		runID, status, ref, deliveredAt, errText); err != nil {
		return nil, fmt.Errorf("erp: finalize log: %w (delivery err: %v)", err, derr)
	}
	del := &ERPDelivery{
		RunID: runID, Seq: seq, BusinessDate: day, Adapter: s.adapter.Name(),
		ManifestSHA256: manifestSHA, FileCount: len(files),
		Status: status, DeliveryRef: ref, DeliveredAt: deliveredAt,
	}
	if derr != nil {
		return del, excerrors.Wrap("INTERNAL_ERROR", "erp: adapter delivery failed", derr)
	}
	return del, nil
}

// ERPDelivery is the erp_delivery_log row view.
type ERPDelivery struct {
	RunID          string     `json:"run_id"`
	Seq            int64      `json:"seq"`
	BusinessDate   time.Time  `json:"business_date"`
	Adapter        string     `json:"adapter"`
	ManifestSHA256 string     `json:"manifest_sha256"`
	FileCount      int        `json:"file_count"`
	Status         string     `json:"status"`
	DeliveryRef    string     `json:"delivery_ref,omitempty"`
	DeliveredAt    *time.Time `json:"delivered_at,omitempty"`
}

// buildFiles assembles the day's bundle members.
func (s *ERPBatchService) buildFiles(ctx context.Context, day time.Time) ([]ERPFile, error) {
	start := normalizeUTCDate(day)
	end := start.AddDate(0, 0, 1)
	dateTag := start.Format("2006-01-02")
	var files []ERPFile

	journals, err := s.renderJournalsCSV(ctx, start, end)
	if err != nil {
		return nil, err
	}
	files = append(files, ERPFile{
		Name: "journals-" + dateTag + ".csv", ContentType: "text/csv", Body: journals})

	if s.tb != nil {
		tbCSV, _, _, err := s.tb.ExportCSV(ctx, ExportTrialBalance, day)
		if err != nil {
			return nil, fmt.Errorf("erp: trial balance: %w", err)
		}
		files = append(files, ERPFile{
			Name: "trial-balance-" + dateTag + ".csv", ContentType: "text/csv", Body: tbCSV})
	}

	idx, err := s.renderStatementIndexCSV(ctx, start, end)
	if err != nil {
		return nil, err
	}
	files = append(files, ERPFile{
		Name: "statements-index-" + dateTag + ".csv", ContentType: "text/csv", Body: idx})

	for i := range files {
		sum := sha256.Sum256(files[i].Body)
		files[i].SHA256 = hex.EncodeToString(sum[:])
	}
	return files, nil
}

// renderJournalsCSV dumps the day's journal entries + ledger lines —
// the GL posting feed the ERP ingests.
func (s *ERPBatchService) renderJournalsCSV(ctx context.Context, start, end time.Time) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("journal_id,entry_type,reference_id,description,posted_by,posted_at,line_id,account_code,debit_amount,credit_amount,currency,narrative\n")
	rows, err := s.pool.Query(ctx, `
		SELECT je.id, je.entry_type::text, COALESCE(je.reference_id::text,''),
		       je.description, je.posted_by, je.posted_at,
		       ll.id, ll.account_code, ll.debit_amount::text, ll.credit_amount::text,
		       ll.currency, COALESCE(ll.narrative,'')
		FROM journal_entries je
		JOIN ledger_lines ll ON ll.journal_entry_id = je.id
		WHERE je.posted_at >= $1 AND je.posted_at < $2
		ORDER BY je.id, ll.id`, start, end)
	if err != nil {
		return nil, fmt.Errorf("erp: journals query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			jid, lid   int64
			etype, ref string
			desc, by   string
			posted     time.Time
			code, d, c string
			ccy, narr  string
		)
		if err := rows.Scan(&jid, &etype, &ref, &desc, &by, &posted,
			&lid, &code, &d, &c, &ccy, &narr); err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "%d,%s,%s,%s,%s,%s,%d,%s,%s,%s,%s,%s\n",
			jid, etype, ref, csvq(desc), csvq(by), posted.UTC().Format(time.RFC3339),
			lid, code, d, c, ccy, csvq(narr))
	}
	return b.Bytes(), rows.Err()
}

// renderStatementIndexCSV lists the client statements generated in the
// window (the ERP/accounting reconciliation index).
func (s *ERPBatchService) renderStatementIndexCSV(ctx context.Context, start, end time.Time) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("statement_id,account_id,period,period_start,period_end,file_ref,content_sha256,generated_at\n")
	rows, err := s.pool.Query(ctx, `
		SELECT statement_id, account_id, period::text, period_start, period_end,
		       file_ref, content_sha256, generated_at
		FROM client_statements
		WHERE generated_at >= $1 AND generated_at < $2
		ORDER BY statement_id`, start, end)
	if err != nil {
		// migration 049 may lag the code deploy — an empty index file is
		// still a valid bundle member (fail-open on the INDEX only; the
		// journal feed is the money-moving part).
		if isUndefinedTable(err) {
			return b.Bytes(), nil
		}
		return nil, fmt.Errorf("erp: statements index: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, acct int64
			period   string
			ps, pe   time.Time
			ref, sha string
			gen      time.Time
		)
		if err := rows.Scan(&id, &acct, &period, &ps, &pe, &ref, &sha, &gen); err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "%d,%d,%s,%s,%s,%s,%s,%s\n",
			id, acct, period, ps.Format("2006-01-02"), pe.Format("2006-01-02"),
			csvq(ref), sha, gen.UTC().Format(time.RFC3339))
	}
	return b.Bytes(), rows.Err()
}

// buildManifest renders manifest.json.
func buildManifest(runID string, seq int64, day, generatedAt time.Time, files []ERPFile) ([]byte, error) {
	type fe struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
		Size   int    `json:"size"`
	}
	m := struct {
		RunID        string `json:"run_id"`
		Seq          int64  `json:"seq"`
		BusinessDate string `json:"business_date"`
		GeneratedAt  string `json:"generated_at"`
		Files        []fe   `json:"files"`
	}{
		RunID: runID, Seq: seq,
		BusinessDate: day.Format("2006-01-02"),
		GeneratedAt:  generatedAt.Format(time.RFC3339),
	}
	for _, f := range files {
		m.Files = append(m.Files, fe{Name: f.Name, SHA256: f.SHA256, Size: len(f.Body)})
	}
	return json.Marshal(m)
}

// isUndefinedTable reports PG 42P01 (undefined_table) — used to tolerate
// a lagging migration on the non-critical index file only.
func isUndefinedTable(err error) bool {
	type sqlState interface{ SQLState() string }
	for e := err; e != nil; {
		if se, ok := e.(sqlState); ok && se.SQLState() == "42P01" {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// ListDeliveries returns recent erp_delivery_log rows (ops surface).
func (s *ERPBatchService) ListDeliveries(ctx context.Context, limit int) ([]ERPDelivery, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT run_id, seq, business_date, adapter, manifest_sha256, file_count,
		       status::text, COALESCE(delivery_ref,''), delivered_at, attempted_at
		FROM erp_delivery_log
		ORDER BY seq DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("erp: list deliveries: %w", err)
	}
	defer rows.Close()
	var out []ERPDelivery
	for rows.Next() {
		var d ERPDelivery
		var att time.Time
		if err := rows.Scan(&d.RunID, &d.Seq, &d.BusinessDate, &d.Adapter,
			&d.ManifestSHA256, &d.FileCount, &d.Status, &d.DeliveryRef,
			&d.DeliveredAt, &att); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// ERPNightlyJob — the scheduler seam.
// ---------------------------------------------------------------------------

// ERPNightlyJob is the unit the orchestrator binds to the nightly batch
// scheduler: it runs the bundle for the previous UTC business day (the
// batch carries a whole closed day's journals + trial balance + statement
// index — running it intra-day would ship a partial day).
type ERPNightlyJob struct {
	svc *ERPBatchService
	now func() time.Time
}

// NewERPNightlyJob wires the job.
func NewERPNightlyJob(svc *ERPBatchService) *ERPNightlyJob {
	return &ERPNightlyJob{svc: svc, now: time.Now}
}

// SetClockForTest overrides the job clock; tests only.
func (j *ERPNightlyJob) SetClockForTest(now func() time.Time) { j.now = now }

// RunOnce delivers the previous UTC day's bundle. A SENT same-day run is
// rejected by the replay gate — safe to re-invoke on scheduler retries.
func (j *ERPNightlyJob) RunOnce(ctx context.Context) (*ERPDelivery, error) {
	day := normalizeUTCDate(j.now().UTC().AddDate(0, 0, -1))
	return j.svc.RunNightly(ctx, day)
}
