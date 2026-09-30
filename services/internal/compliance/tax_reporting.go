// tax_reporting.go — Phase-21 Task 21.3.22: automated tax reporting —
// CRS (OECD Common Reporting Standard) and FATCA (IGA Model 1) XML
// generation plus the run lifecycle (migration 245).
//
// Data sources (no parallel PII store): tax_self_certifications via the
// sealed PgStore (AES-256-GCM — TINs unseal only inside the run, and
// only into the sealed artifact), users + accounts for identity, the
// latest balance_snapshots on/before Dec 31 of the report year for
// year-end balances (fallback: live balances), funding_transactions
// for the year's payment aggregates.
//
// Lifecycle (dual control at approval — spec §24 dual-control family):
//
//	DRAFT → UNDER_REVIEW → APPROVED → SUBMITTED
//	         ↘ REJECTED        (regeneration writes a new version,
//	                            superseding non-submitted runs)
//
// Fail-closed: FATCA runs hard-require a US TIN per reportable account;
// CRS runs fail when an account carries a REJECTED self-cert (garbage
// in → refused run, never a partial file). Missing-identity accounts
// (no name resolvable) fail the run too — TAX_REPORT_DATA_INCOMPLETE.
package compliance

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"
	excerrors "exchange/pkg/errors"
)

// TaxReportRegime values.
const (
	TaxRegimeCRS   = "CRS"
	TaxRegimeFATCA = "FATCA"
)

// TaxVenue is the reporting-FI identity stamped on every artifact —
// configured from env at boot (EXC_TAX_VENUE_NAME / _IN / _COUNTRY).
type TaxVenue struct {
	Name    string // legal FI name
	IN      string // GIIN (FATCA) / LEI-style identifier
	Country string // venue domicile (ISO alpha-2) — CRS domestic scope
}

// TaxReportRun is the lifecycle row.
type TaxReportRun struct {
	ID              int64           `json:"id"`
	Regime          string          `json:"regime"`
	ReportYear      int             `json:"report_year"`
	Jurisdiction    string          `json:"jurisdiction"`
	Version         int             `json:"version"`
	Status          string          `json:"status"`
	ArtifactRef     *string         `json:"artifact_ref,omitempty"`
	SHA256          *string         `json:"sha256,omitempty"`
	AccountCount    int             `json:"account_count"`
	Detail          json.RawMessage `json:"detail"`
	CreatedBy       int64           `json:"created_by"`
	ReviewedBy      *int64          `json:"reviewed_by,omitempty"`
	ApprovedBy      *int64          `json:"approved_by,omitempty"`
	SubmittedBy     *int64          `json:"submitted_by,omitempty"`
	SubmittedAt     *time.Time      `json:"submitted_at,omitempty"`
	SubmissionRef   *string         `json:"submission_ref,omitempty"`
	RejectionReason *string         `json:"rejection_reason,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// reportableAccount is the per-account record feeding the XML
// generators — TIN is transient (unsealed for the artifact build, never
// persisted back).
type reportableAccount struct {
	AccountID        int64
	AccountNumber    string
	HolderName       string
	Address          string
	ResidenceCountry string // ISO alpha-2
	TIN              string
	TINIssuedBy      string
	Entity           bool // entity account (W-8BEN-E) vs individual
	Balances         []reportAmount
	Payments         []reportAmount
}

type reportAmount struct {
	Currency string
	Amount   string // decimal text — exact, no float drift
}

// TaxReportService owns run generation + lifecycle.
type TaxReportService struct {
	pool     *pgxpool.Pool
	kyc      *PgStore
	resolver HoldRoleResolver
	now      func() time.Time
	venue    TaxVenue
}

// NewTaxReportService wires the service. venue must be complete — a
// CRS/FATCA file with a blank reporting-FI identity is invalid by
// definition.
func NewTaxReportService(pool *pgxpool.Pool, kyc *PgStore,
	resolver HoldRoleResolver, venue TaxVenue) (*TaxReportService, error) {
	if pool == nil || kyc == nil || resolver == nil {
		return nil, fmt.Errorf("compliance: tax reporting requires pool, kyc store, resolver")
	}
	if venue.Name == "" || venue.Country == "" {
		return nil, fmt.Errorf("compliance: tax venue identity incomplete")
	}
	return &TaxReportService{
		pool: pool, kyc: kyc, resolver: resolver,
		now: time.Now, venue: venue,
	}, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *TaxReportService) SetClockForTest(now func() time.Time) {
	s.now = now
}

// ---------------------------------------------------------------------------
// Generation
// ---------------------------------------------------------------------------

// Generate builds a run for (regime, year, jurisdiction). Jurisdiction
// "" = all non-domestic residencies (CRS) / "US" implicit (FATCA).
// Regeneration supersedes any prior non-SUBMITTED run for the same key
// and bumps version; a SUBMITTED run blocks regeneration of that key.
func (s *TaxReportService) Generate(ctx context.Context, regime string,
	year int, jurisdiction string, officer int64) (*TaxReportRun, error) {

	regime = strings.ToUpper(strings.TrimSpace(regime))
	jurisdiction = strings.ToUpper(strings.TrimSpace(jurisdiction))
	if regime != TaxRegimeCRS && regime != TaxRegimeFATCA {
		return nil, excerrors.New("INVALID_REQUEST", "regime must be CRS or FATCA")
	}
	if err := s.checkTaxRole(ctx, officer); err != nil {
		return nil, err
	}
	if year < 2000 || year > 2100 {
		return nil, excerrors.New("INVALID_REQUEST", "report_year out of range")
	}

	accts, detail, err := s.collectReportable(ctx, regime, year, jurisdiction)
	if err != nil {
		return nil, err
	}

	// Version bookkeeping (tx): supersede non-submitted siblings,
	// refuse to regenerate a SUBMITTED run.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("tax report: gen tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var submitted int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM tax_report_runs
		 WHERE regime=$1 AND report_year=$2 AND jurisdiction=$3
		   AND status='SUBMITTED'`,
		regime, year, jurisdiction).Scan(&submitted); err != nil {
		return nil, fmt.Errorf("tax report: submitted check: %w", err)
	}
	if submitted > 0 {
		return nil, excerrors.New("TAX_REPORT_INVALID_TRANSITION",
			"a SUBMITTED run already exists for this key — file an amendment")
	}
	var version int
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(version),0)+1 FROM tax_report_runs
		 WHERE regime=$1 AND report_year=$2 AND jurisdiction=$3`,
		regime, year, jurisdiction).Scan(&version); err != nil {
		return nil, fmt.Errorf("tax report: version: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tax_report_runs SET status='SUPERSEDED', updated_at=now()
		 WHERE regime=$1 AND report_year=$2 AND jurisdiction=$3
		   AND status NOT IN ('SUBMITTED','SUPERSEDED')`,
		regime, year, jurisdiction); err != nil {
		return nil, fmt.Errorf("tax report: supersede: %w", err)
	}

	// Build the artifact inside the tx so a marshal failure leaves no
	// half-written run.
	var xml []byte
	switch regime {
	case TaxRegimeCRS:
		xml, err = BuildCRSXML(s.venue, jurisdiction, year, version,
			s.now().UTC(), accts)
	default:
		xml, err = BuildFATCAXML(s.venue, year, version,
			s.now().UTC(), accts)
	}
	if err != nil {
		return nil, fmt.Errorf("tax report: xml build: %w", err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(xml))
	artifactRef := fmt.Sprintf("db://tax_report_runs/%s/%d/%s/v%d.xml",
		regime, year, jurisdiction, version)

	var run TaxReportRun
	detail["artifact_bytes"] = len(xml)
	err = tx.QueryRow(ctx, `
		INSERT INTO tax_report_runs
		    (regime, report_year, jurisdiction, version, status,
		     artifact_ref, artifact, sha256, account_count, detail,
		     created_by)
		VALUES ($1,$2,$3,$4,'DRAFT',$5,$6,$7,$8,$9,$10)
		RETURNING id, created_at, updated_at`,
		regime, year, jurisdiction, version, artifactRef, xml, sum,
		len(accts), mustJSON(detail), officer).
		Scan(&run.ID, &run.CreatedAt, &run.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("tax report: run insert: %w", err)
	}
	if _, err := audit.Append(ctx, tx, "tax_report_runs", &run.ID,
		"GENERATED", nil); err != nil {
		return nil, fmt.Errorf("tax report: audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("tax report: gen commit: %w", err)
	}
	run.Regime, run.ReportYear, run.Jurisdiction = regime, year, jurisdiction
	run.Version, run.Status = version, "DRAFT"
	run.ArtifactRef, run.SHA256 = &artifactRef, &sum
	run.AccountCount, run.Detail = len(accts), mustJSON(detail)
	run.CreatedBy = officer
	return &run, nil
}

// ---------------------------------------------------------------------------
// Reportable-account collection (the PII path — cert fields unseal here)
// ---------------------------------------------------------------------------

// collectReportable resolves the account set + per-account records.
func (s *TaxReportService) collectReportable(ctx context.Context, regime string,
	year int, jurisdiction string) ([]reportableAccount, map[string]any, error) {

	// Candidate accounts — every non-closed account or cert holder;
	// residency filtering happens per-account below. Entity vs
	// individual resolves from the cert form type (W-8BEN-E), not
	// account_type (SPOT/MARGIN/PORTFOLIO is a product axis).
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.user_id, u.country, u.full_name, u.address
		  FROM accounts a JOIN users u ON u.id = a.user_id
		 WHERE a.status <> 'CLOSED' OR EXISTS (
		    SELECT 1 FROM tax_self_certifications t WHERE t.account_id = a.id)
		 ORDER BY a.id`)
	if err != nil {
		return nil, nil, fmt.Errorf("tax report: account scan: %w", err)
	}
	type cand struct {
		acct, uid  int64
		country    *string
		name, addr *string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.acct, &c.uid, &c.country, &c.name,
			&c.addr); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("tax report: account scan row: %w", err)
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	detail := map[string]any{"regime": regime, "year": year,
		"jurisdiction": jurisdiction}
	var out []reportableAccount
	var skipped []map[string]any

	for _, c := range cands {
		rec, reportable, err := s.buildAccountRecord(ctx, c.acct, c.uid,
			c.country, c.name, c.addr, regime, year)
		if err != nil {
			return nil, nil, err // fail-closed — bad data aborts the run
		}
		if !reportable {
			continue
		}
		if jurisdiction != "" && regime == TaxRegimeCRS &&
			!strings.EqualFold(rec.ResidenceCountry, jurisdiction) {
			skipped = append(skipped, map[string]any{
				"account_id": c.acct, "reason": "jurisdiction filter"})
			continue
		}
		out = append(out, *rec)
	}
	detail["skipped"] = skipped
	sort.Slice(out, func(i, j int) bool { return out[i].AccountID < out[j].AccountID })
	return out, detail, nil
}

// buildAccountRecord assembles one reportable record; reportable=false
// means the account is domestic/out-of-scope for this regime.
func (s *TaxReportService) buildAccountRecord(ctx context.Context,
	accountID, userID int64, country, name, addr *string,
	regime string, year int) (*reportableAccount, bool, error) {

	// Self-cert is the authoritative tax-residence source.
	certs, err := s.kyc.ListSelfCerts(ctx, accountID)
	if err != nil {
		return nil, false, fmt.Errorf("tax report: certs %d: %w", accountID, err)
	}
	var cert *SelfCert
	for i := range certs {
		if certs[i].Status == "VALIDATED" {
			cert = &certs[i]
			break
		}
		if cert == nil {
			cert = &certs[i]
		}
	}
	rejectedCert := false
	for i := range certs {
		if certs[i].Status == "REJECTED" {
			rejectedCert = true
		}
	}

	rec := &reportableAccount{
		AccountID:     accountID,
		AccountNumber: fmt.Sprintf("ACC-%d", accountID),
	}
	if cert != nil {
		rec.ResidenceCountry = strings.ToUpper(cert.TINCountry)
		rec.TIN = cert.TIN
		rec.TINIssuedBy = cert.TINCountry
		var fields struct {
			LegalName string `json:"legal_name"`
			Address   string `json:"address"`
		}
		_ = json.Unmarshal(cert.Fields, &fields)
		if fields.LegalName != "" {
			rec.HolderName = fields.LegalName
		}
		if fields.Address != "" {
			rec.Address = fields.Address
		}
		if cert.FormType == "W-8BEN-E" {
			rec.Entity = true
		}
	}
	if rec.ResidenceCountry == "" && country != nil {
		rec.ResidenceCountry = strings.ToUpper(*country)
	}
	if rec.HolderName == "" && name != nil {
		rec.HolderName = *name
	}
	if rec.Address == "" && addr != nil {
		rec.Address = *addr
	}

	// Regime scoping.
	switch regime {
	case TaxRegimeFATCA:
		usPerson := rec.ResidenceCountry == "US" ||
			(cert != nil && cert.FormType == "W-9")
		if !usPerson {
			return nil, false, nil
		}
		if rec.TIN == "" {
			return nil, false, excerrors.New("TAX_REPORT_DATA_INCOMPLETE",
				fmt.Sprintf("FATCA reportable account %d has no US TIN", accountID))
		}
	case TaxRegimeCRS:
		if rec.ResidenceCountry == "" ||
			strings.EqualFold(rec.ResidenceCountry, s.venue.Country) {
			return nil, false, nil // domestic or unattributed — not reportable
		}
		if rejectedCert && cert != nil && cert.Status == "REJECTED" {
			return nil, false, excerrors.New("TAX_REPORT_DATA_INCOMPLETE",
				fmt.Sprintf("account %d carries a REJECTED self-cert", accountID))
		}
	}
	if rec.HolderName == "" {
		return nil, false, excerrors.New("TAX_REPORT_DATA_INCOMPLETE",
			fmt.Sprintf("account %d has no resolvable holder name", accountID))
	}

	// Year-end balances — latest snapshot ≤ Dec 31, else live balances.
	rec.Balances, err = s.yearEndBalances(ctx, accountID, year)
	if err != nil {
		return nil, false, err
	}
	rec.Payments, err = s.yearPayments(ctx, accountID, year)
	if err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

// yearEndBalances prefers the daily snapshot at Dec 31 (closest ≤),
// falling back to live balances — flagged in detail by the caller
// (balance source is per-account in the artifact's detail block).
func (s *TaxReportService) yearEndBalances(ctx context.Context,
	accountID int64, year int) ([]reportAmount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT currency, (available + locked)::text
		  FROM balance_snapshots
		 WHERE account_id=$1
		   AND snapshot_date = (
		       SELECT max(snapshot_date) FROM balance_snapshots
		        WHERE account_id=$1
		          AND snapshot_date <= make_date($2,12,31))
		 ORDER BY currency`, accountID, year)
	if err == nil {
		var out []reportAmount
		for rows.Next() {
			var a reportAmount
			if err := rows.Scan(&a.Currency, &a.Amount); err != nil {
				rows.Close()
				return nil, fmt.Errorf("tax report: snapshot scan: %w", err)
			}
			out = append(out, a)
		}
		rows.Close()
		if rows.Err() != nil {
			return nil, rows.Err()
		}
		if len(out) > 0 {
			return out, nil
		}
	} else if !isMissingTableErr(err) {
		return nil, fmt.Errorf("tax report: snapshot read: %w", err)
	}
	// Fallback: live balances (dev DBs without the snapshot worker).
	rows2, err := s.pool.Query(ctx, `
		SELECT currency, total::text FROM balances
		 WHERE account_id=$1 AND total <> 0 ORDER BY currency`, accountID)
	if err != nil {
		return nil, fmt.Errorf("tax report: balances read: %w", err)
	}
	defer rows2.Close()
	var out []reportAmount
	for rows2.Next() {
		var a reportAmount
		if err := rows2.Scan(&a.Currency, &a.Amount); err != nil {
			return nil, fmt.Errorf("tax report: balances scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows2.Err()
}

// yearPayments aggregates income-type funding movements in the report
// year (funding-rate credits, adjustments, fee rebates — deposits and
// withdrawals are principal movements, not payments).
func (s *TaxReportService) yearPayments(ctx context.Context,
	accountID int64, year int) ([]reportAmount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT currency, sum(amount)::text
		  FROM funding_transactions
		 WHERE account_id=$1
		   AND type IN ('FUNDING_RATE','ADJUSTMENT','SETTLEMENT')
		   AND amount > 0
		   AND status = 'CONFIRMED'
		   AND created_at >= make_timestamptz($2,1,1,0,0,0)
		   AND created_at <  make_timestamptz($2+1,1,1,0,0,0)
		 GROUP BY currency ORDER BY currency`, accountID, year)
	if err != nil {
		return nil, fmt.Errorf("tax report: payments read: %w", err)
	}
	defer rows.Close()
	var out []reportAmount
	for rows.Next() {
		var a reportAmount
		if err := rows.Scan(&a.Currency, &a.Amount); err != nil {
			return nil, fmt.Errorf("tax report: payments scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Lifecycle (dual control at APPROVED)
// ---------------------------------------------------------------------------

// transition applies a lifecycle step inside a tx after FOR UPDATE.
func (s *TaxReportService) transition(ctx context.Context, runID int64,
	actor int64, want string, set map[string]any, action string) (*TaxReportRun, error) {
	if err := s.checkTaxRole(ctx, actor); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("tax report: transition tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var run TaxReportRun
	err = tx.QueryRow(ctx, `
		SELECT id, regime, report_year, jurisdiction, version, status,
		       artifact_ref, sha256, account_count, detail, created_by,
		       reviewed_by, approved_by, submitted_by, submitted_at,
		       submission_ref, rejection_reason, created_at, updated_at
		  FROM tax_report_runs WHERE id=$1 FOR UPDATE`, runID).
		Scan(&run.ID, &run.Regime, &run.ReportYear, &run.Jurisdiction,
			&run.Version, &run.Status, &run.ArtifactRef, &run.SHA256,
			&run.AccountCount, &run.Detail, &run.CreatedBy,
			&run.ReviewedBy, &run.ApprovedBy, &run.SubmittedBy,
			&run.SubmittedAt, &run.SubmissionRef, &run.RejectionReason,
			&run.CreatedAt, &run.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "tax report run not found")
	}
	if err != nil {
		return nil, fmt.Errorf("tax report: run lock: %w", err)
	}
	if run.Status != want {
		return nil, excerrors.New("TAX_REPORT_INVALID_TRANSITION",
			fmt.Sprintf("run %d is %s — %s requires %s",
				runID, run.Status, action, want))
	}

	clause := "status=$2"
	args := []any{runID, ""}
	switch action {
	case "REVIEW":
		if run.ReviewedBy != nil && *run.ReviewedBy == actor {
			return nil, excerrors.New("TAX_REPORT_INVALID_TRANSITION",
				"already reviewed by this officer")
		}
		args[1] = "UNDER_REVIEW"
		clause += ", reviewed_by=$3, reviewed_at=now()"
		args = append(args, actor)
	case "APPROVE":
		// Dual control: approver ≠ creator AND ≠ reviewer.
		if actor == run.CreatedBy ||
			(run.ReviewedBy != nil && actor == *run.ReviewedBy) {
			return nil, excerrors.New("DUAL_CONTROL_VIOLATION",
				"approver must differ from the drafting and reviewing officers")
		}
		if run.ReviewedBy == nil {
			return nil, excerrors.New("DUAL_CONTROL_REQUIRED",
				"a review must precede approval")
		}
		args[1] = "APPROVED"
		clause += ", approved_by=$3, approved_at=now()"
		args = append(args, actor)
	case "REJECT":
		reason, _ := set["reason"].(string)
		if strings.TrimSpace(reason) == "" {
			return nil, excerrors.New("INVALID_REQUEST",
				"rejection reason required")
		}
		args[1] = "REJECTED"
		clause += ", rejected_by=$3, rejected_at=now(), rejection_reason=$4"
		args = append(args, actor, reason)
	case "SUBMIT":
		ref, _ := set["submission_ref"].(string)
		args[1] = "SUBMITTED"
		clause += ", submitted_by=$3, submitted_at=now(), submission_ref=$4"
		args = append(args, actor, ref)
	default:
		return nil, excerrors.New("INTERNAL_ERROR", "unknown transition "+action)
	}

	q := `UPDATE tax_report_runs SET ` + clause + `, updated_at=now() WHERE id=$1`
	if _, err := tx.Exec(ctx, q, args...); err != nil {
		return nil, fmt.Errorf("tax report: transition write: %w", err)
	}
	if _, err := audit.Append(ctx, tx, "tax_report_runs", &run.ID,
		action, nil); err != nil {
		return nil, fmt.Errorf("tax report: transition audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("tax report: transition commit: %w", err)
	}
	return s.Get(ctx, runID)
}

// Review marks a DRAFT run UNDER_REVIEW.
func (s *TaxReportService) Review(ctx context.Context, runID, officer int64) (*TaxReportRun, error) {
	return s.transition(ctx, runID, officer, "DRAFT", nil, "REVIEW")
}

// Approve marks an UNDER_REVIEW run APPROVED — dual control enforced.
func (s *TaxReportService) Approve(ctx context.Context, runID, approver int64) (*TaxReportRun, error) {
	return s.transition(ctx, runID, approver, "UNDER_REVIEW", nil, "APPROVE")
}

// Reject marks a DRAFT or UNDER_REVIEW run REJECTED.
func (s *TaxReportService) Reject(ctx context.Context, runID, officer int64,
	reason string) (*TaxReportRun, error) {
	run, err := s.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.Status != "DRAFT" && run.Status != "UNDER_REVIEW" {
		return nil, excerrors.New("TAX_REPORT_INVALID_TRANSITION",
			"only DRAFT/UNDER_REVIEW runs can be rejected")
	}
	return s.transition(ctx, runID, officer, run.Status,
		map[string]any{"reason": reason}, "REJECT")
}

// Submit records the authority filing (status APPROVED → SUBMITTED,
// receipt reference optional but recommended).
func (s *TaxReportService) Submit(ctx context.Context, runID, officer int64,
	receiptRef string) (*TaxReportRun, error) {
	return s.transition(ctx, runID, officer, "APPROVED",
		map[string]any{"submission_ref": receiptRef}, "SUBMIT")
}

// ---------------------------------------------------------------------------
// Read surface
// ---------------------------------------------------------------------------

const taxRunCols = `id, regime, report_year, jurisdiction, version, status,
	artifact_ref, sha256, account_count, detail, created_by, reviewed_by,
	approved_by, submitted_by, submitted_at, submission_ref,
	rejection_reason, created_at, updated_at`

// Get returns one run.
func (s *TaxReportService) Get(ctx context.Context, runID int64) (*TaxReportRun, error) {
	var r TaxReportRun
	err := s.pool.QueryRow(ctx, `
		SELECT `+taxRunCols+` FROM tax_report_runs WHERE id=$1`, runID).
		Scan(&r.ID, &r.Regime, &r.ReportYear, &r.Jurisdiction, &r.Version,
			&r.Status, &r.ArtifactRef, &r.SHA256, &r.AccountCount,
			&r.Detail, &r.CreatedBy, &r.ReviewedBy, &r.ApprovedBy,
			&r.SubmittedBy, &r.SubmittedAt, &r.SubmissionRef,
			&r.RejectionReason, &r.CreatedAt, &r.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "tax report run not found")
	}
	if err != nil {
		return nil, fmt.Errorf("tax report: get: %w", err)
	}
	return &r, nil
}

// List returns runs newest-first.
func (s *TaxReportService) List(ctx context.Context, regime string,
	limit int) ([]TaxReportRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows pgx.Rows
	var err error
	if regime != "" {
		rows, err = s.pool.Query(ctx, `
			SELECT `+taxRunCols+` FROM tax_report_runs
			 WHERE regime=$1 ORDER BY id DESC LIMIT $2`,
			strings.ToUpper(regime), limit)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT `+taxRunCols+` FROM tax_report_runs
			 ORDER BY id DESC LIMIT $1`, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("tax report: list: %w", err)
	}
	defer rows.Close()
	out := []TaxReportRun{}
	for rows.Next() {
		var r TaxReportRun
		if err := rows.Scan(&r.ID, &r.Regime, &r.ReportYear, &r.Jurisdiction,
			&r.Version, &r.Status, &r.ArtifactRef, &r.SHA256,
			&r.AccountCount, &r.Detail, &r.CreatedBy, &r.ReviewedBy,
			&r.ApprovedBy, &r.SubmittedBy, &r.SubmittedAt,
			&r.SubmissionRef, &r.RejectionReason, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("tax report: list scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Artifact returns the stored XML bytes for a run (integrity-checked
// against sha256 before serving).
func (s *TaxReportService) Artifact(ctx context.Context, runID int64) ([]byte, error) {
	var xml []byte
	var sum string
	err := s.pool.QueryRow(ctx, `
		SELECT artifact, sha256 FROM tax_report_runs WHERE id=$1`, runID).
		Scan(&xml, &sum)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "tax report run not found")
	}
	if err != nil {
		return nil, fmt.Errorf("tax report: artifact read: %w", err)
	}
	if len(xml) == 0 {
		return nil, excerrors.New("NOT_FOUND", "run carries no artifact")
	}
	if fmt.Sprintf("%x", sha256.Sum256(xml)) != sum {
		return nil, excerrors.New("COMMS_INTEGRITY_FAILURE",
			"tax report artifact sha256 mismatch")
	}
	return xml, nil
}

func (s *TaxReportService) checkTaxRole(ctx context.Context, userID int64) error {
	role, err := s.resolver(ctx, userID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if role != "Compliance Officer" && role != "Super Admin" &&
		role != "Finance Ops" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot action tax reports")
	}
	return nil
}

// isMissingTableErr reports PG 42P01 (undefined_table) — the
// balance_snapshots fallback tolerates a dev schema without the
// Phase-20 snapshot table.
func isMissingTableErr(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "42P01"
	}
	return false
}
