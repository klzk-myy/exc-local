// segregation_cert.go — Phase-24 Task 24.3.18 items 2/3: the evidence pack
// and the segregation certification (spec §17.13.2, §24 #330).
//
//   - AssembleEvidencePack builds the auditor pack REPEATEDLY from the
//     system of record — daily segregation reconciliations and sign-offs,
//     bank-reconciliation outcomes, the shortfall top-up log, the GL lines
//     on client-segregation account codes and the §17.11 Merkle
//     proof-of-reserves roots for each attested day. The pack is persisted
//     with its sha256 so a certification binds to an exact byte stream —
//     it is never hand-assembled.
//   - IssueCertification records the issued certification (dual control:
//     issued_by ≠ approved_by) over an attested period with the pack hash
//     and published_until.
//   - AssertReleaseGate is the Phase-24 → production release-gate seam: a
//     covered period with a missing or expired certification fails closed.
package backoffice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/admin"
	"exchange/internal/ledger"
	excerrors "exchange/pkg/errors"
)

// Certification statuses (segregation_certifications.status).
const (
	CertIssued  = "ISSUED"
	CertExpired = "EXPIRED"
	CertRevoked = "REVOKED"
)

// EvidencePack is one client_money_evidence_packs row — the hashed,
// system-assembled auditor artefact.
type EvidencePack struct {
	ID          int64           `json:"id"`
	AuditID     int64           `json:"audit_id"`
	PeriodStart time.Time       `json:"period_start"`
	PeriodEnd   time.Time       `json:"period_end"`
	Pack        json.RawMessage `json:"pack"`
	PackSHA256  string          `json:"pack_sha256"`
	AssembledBy int64           `json:"assembled_by"`
	AssembledAt time.Time       `json:"assembled_at"`
}

// SegregationCertification is one segregation_certifications row — the
// artefact satisfying §17.9's legal segregation obligation.
type SegregationCertification struct {
	ID             int64           `json:"id"`
	AuditID        int64           `json:"audit_id"`
	EvidencePackID *int64          `json:"evidence_pack_id,omitempty"`
	PeriodStart    time.Time       `json:"period_start"`
	PeriodEnd      time.Time       `json:"period_end"`
	Statement      string          `json:"statement"`
	Signatories    json.RawMessage `json:"signatories"`
	PackSHA256     string          `json:"pack_sha256"`
	IssuedBy       int64           `json:"issued_by"`
	ApprovedBy     int64           `json:"approved_by"`
	PublishedUntil time.Time       `json:"published_until"`
	Status         string          `json:"status"`
	CreatedAt      time.Time       `json:"created_at"`
}

// clientGLPrefixes are the client-segregation chart ranges contributing
// GL lines to the evidence pack (ledger.SegregationOf owns the rule).
var clientGLPrefixes = []string{
	"1110_CLIENT_MONEY_SEGREGATED", // segregated bank mirror
	"2010_CUSTOMER_LIABILITY",      // client entitlement liability
	"2011_PENDING_SETTLEMENT_DELIVERY",
	"2100_CLIENT_COLLATERAL",
	"2150_SUSPENSE_DEPOSITS",
	"2160_CLEARING_TRANSIT",
	"2170_PAMM_POOL_LIABILITY",
}

// clientGLCodes expands the prefixes over the seeded chart currencies.
func clientGLCodes() []string {
	out := make([]string, 0, len(clientGLPrefixes)*len(ledger.SeedCurrencies))
	for _, p := range clientGLPrefixes {
		for _, ccy := range ledger.SeedCurrencies {
			out = append(out, p+"_"+ccy)
		}
	}
	return out
}

// AssembleEvidencePack builds and persists the auditor pack for an
// engagement — FIELDWORK or DRAFT only (a pack against a SCHEDULED
// engagement predates evidence; against ISSUED it would rewrite history).
// Contents come exclusively from the system of record:
//
//	reconciliations      daily segregation calcs + sign-offs (Task 24.3.11)
//	bank_reconciliation  external-leg outcomes carried on those rows
//	topup_log            remediation records (the 4-tier waterfall log)
//	breaks               safeguarding breaks in the window
//	gl_lines             ledger lines on client-segregation account codes
//	por_roots            §17.11 Merkle proof-of-reserves root per day
func (s *AssuranceService) AssembleEvidencePack(ctx context.Context, actor admin.AdminActor, auditID int64) (*EvidencePack, error) {
	if err := s.requireRole(ctx, actor.UserID, complianceOrFinance); err != nil {
		return nil, err
	}
	var out *EvidencePack
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		a, err := tx.AuditByID(ctx, auditID)
		if err != nil {
			return err
		}
		if a == nil {
			return excerrors.New("NOT_FOUND", "audit engagement not found")
		}
		if a.Status != AuditFieldwork && a.Status != AuditDraft {
			return excerrors.New("INVALID_LIFECYCLE_TRANSITION",
				fmt.Sprintf("evidence packs assemble during FIELDWORK/DRAFT — audit %d is %s", auditID, a.Status))
		}
		recons, err := tx.ListReconciliations(ctx, a.PeriodStart, a.PeriodEnd)
		if err != nil {
			return err
		}
		rems, err := tx.ListRemediations(ctx, a.PeriodStart, a.PeriodEnd)
		if err != nil {
			return err
		}
		breaks, err := tx.ListBreaks(ctx, a.PeriodStart, a.PeriodEnd)
		if err != nil {
			return err
		}
		glLines, err := tx.GLLines(ctx, clientGLCodes(), a.PeriodStart, a.PeriodEnd.Add(24*time.Hour))
		if err != nil {
			return err
		}
		roots, err := tx.MerkleRoots(ctx, a.PeriodStart, a.PeriodEnd)
		if err != nil {
			return err
		}
		stressRuns, err := tx.ListStressRuns(ctx, a.PeriodStart, a.PeriodEnd)
		if err != nil {
			return err
		}
		// Map keys render in sorted order under encoding/json — the pack
		// bytes are deterministic for identical source state.
		pack := map[string]any{
			"audit_id":             auditID,
			"period_start":         a.PeriodStart.Format("2006-01-02"),
			"period_end":           a.PeriodEnd.Format("2006-01-02"),
			"assembled_at":         s.now().Format(time.RFC3339Nano),
			"reconciliations":      recons,
			"topup_log":            rems,
			"breaks":               breaks,
			"stress_runs":          stressRuns,
			"gl_lines":             glLines,
			"por_roots":            roots,
			"bank_reconciliations": externalLeg(recons),
		}
		blob, err := json.Marshal(pack)
		if err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "evidence pack encode", err)
		}
		sum := sha256.Sum256(blob)
		row, err := tx.InsertEvidencePack(ctx, EvidencePack{
			AuditID: auditID, PeriodStart: a.PeriodStart, PeriodEnd: a.PeriodEnd,
			Pack: blob, PackSHA256: hex.EncodeToString(sum[:]),
			AssembledBy: actor.UserID,
		})
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	return out, err
}

// externalLeg extracts the bank-reconciliation outcome summary from each
// reconciliation row (the Task 24.3.12 statement comparison lives in the
// reconciliation's external_snapshot — never re-derived here).
func externalLeg(recons []Reconciliation) []map[string]any {
	out := []map[string]any{}
	for _, r := range recons {
		out = append(out, map[string]any{
			"reconciliation_id": r.ID,
			"recon_date":        r.ReconDate.Format("2006-01-02"),
			"currency":          r.Currency,
			"external_status":   r.ExternalStatus,
			"external_snapshot": json.RawMessage(r.ExternalSnapshot),
		})
	}
	return out
}

// IssueCertification records the segregation certification over an
// attested period — dual control: issued_by (actor) ≠ approved_by
// (actor.ApproverID), both compliance-eligible. The certification binds to
// an evidence pack: pack_sha256 is carried verbatim so a re-assembled pack
// can be verified byte-for-byte. Issuing also flips the engagement to
// ISSUED.
func (s *AssuranceService) IssueCertification(ctx context.Context, actor admin.AdminActor,
	auditID, packID int64, statement string, signatories json.RawMessage,
	publishedUntil time.Time) (*SegregationCertification, error) {
	if err := s.requireRole(ctx, actor.UserID, complianceOrFinance); err != nil {
		return nil, err
	}
	if actor.ApproverID <= 0 {
		return nil, excerrors.New("DUAL_CONTROL_REQUIRED",
			"certification issuance requires a distinct approver_id")
	}
	if actor.ApproverID == actor.UserID {
		return nil, excerrors.New("DUAL_CONTROL_VIOLATION",
			"certifier and approver must be distinct principals")
	}
	if err := s.requireRole(ctx, actor.ApproverID, complianceOrFinance); err != nil {
		return nil, err
	}
	if statement == "" || !publishedUntil.After(s.now()) {
		return nil, excerrors.New("INVALID_REQUEST",
			"certification requires a statement and a future published_until")
	}
	if len(signatories) == 0 || !json.Valid(signatories) {
		return nil, excerrors.New("INVALID_REQUEST", "signatories must be valid JSON")
	}
	var out *SegregationCertification
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		a, err := tx.AuditByID(ctx, auditID)
		if err != nil {
			return err
		}
		if a == nil {
			return excerrors.New("NOT_FOUND", "audit engagement not found")
		}
		pack, err := tx.EvidencePackByID(ctx, packID)
		if err != nil {
			return err
		}
		if pack == nil || pack.AuditID != auditID {
			return excerrors.New("NOT_FOUND",
				"evidence pack not found for this engagement — certifications bind to system-assembled packs")
		}
		cert, err := tx.InsertCertification(ctx, SegregationCertification{
			AuditID: auditID, EvidencePackID: &packID,
			PeriodStart: a.PeriodStart, PeriodEnd: a.PeriodEnd,
			Statement: statement, Signatories: signatories,
			PackSHA256: pack.PackSHA256, IssuedBy: actor.UserID,
			ApprovedBy: actor.ApproverID, PublishedUntil: publishedUntil,
			Status: CertIssued,
		})
		if err != nil {
			return err
		}
		if a.Status == AuditDraft || a.Status == AuditFieldwork {
			a.Status = AuditIssued
			if err := tx.UpdateAudit(ctx, *a); err != nil {
				return err
			}
		}
		out = cert
		return nil
	})
	return out, err
}

// ListCertifications returns all certification rows (read surface).
func (s *AssuranceService) ListCertifications(ctx context.Context, actor admin.AdminActor) ([]SegregationCertification, error) {
	if err := s.requireRole(ctx, actor.UserID, readRoles); err != nil {
		return nil, err
	}
	return s.store.ListCertifications(ctx)
}

// AssertReleaseGate is the Phase-24 → production release-gate seam: the
// covered period ending day must carry an ISSUED certification whose
// published_until has not lapsed. Missing or expired coverage fails
// closed — FORBIDDEN (the gate is a launch precondition, not a client
// route).
func (s *AssuranceService) AssertReleaseGate(ctx context.Context, day time.Time) error {
	certs, err := s.store.CertificationsCovering(ctx, day)
	if err != nil {
		return err
	}
	now := s.now()
	for _, c := range certs {
		if c.Status == CertIssued && now.Before(c.PublishedUntil) {
			return nil
		}
	}
	return excerrors.New("FORBIDDEN", fmt.Sprintf(
		"no current segregation certification covers %s — Phase-24 release gate blocked (spec §17.13.2)",
		day.Format("2006-01-02")))
}

// --- PgStore: evidence packs + certifications ---------------------------------

const packCols = `id, audit_id, period_start, period_end, pack, pack_sha256,
	assembled_by, assembled_at`

func scanPack(row pgx.Row) (*EvidencePack, error) {
	var p EvidencePack
	if err := row.Scan(&p.ID, &p.AuditID, &p.PeriodStart, &p.PeriodEnd,
		&p.Pack, &p.PackSHA256, &p.AssembledBy, &p.AssembledAt); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *PgStore) InsertEvidencePack(ctx context.Context, p EvidencePack) (*EvidencePack, error) {
	return scanPack(s.q.QueryRow(ctx, `
		INSERT INTO client_money_evidence_packs
		 (audit_id, period_start, period_end, pack, pack_sha256, assembled_by)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+packCols,
		p.AuditID, p.PeriodStart, p.PeriodEnd, p.Pack, p.PackSHA256, p.AssembledBy))
}

func (s *PgStore) EvidencePackByID(ctx context.Context, id int64) (*EvidencePack, error) {
	p, err := scanPack(s.q.QueryRow(ctx,
		`SELECT `+packCols+` FROM client_money_evidence_packs WHERE id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return p, err
}

func (s *PgStore) EvidencePacksForAudit(ctx context.Context, auditID int64) ([]EvidencePack, error) {
	rows, err := s.q.Query(ctx,
		`SELECT `+packCols+` FROM client_money_evidence_packs
		  WHERE audit_id=$1 ORDER BY assembled_at DESC`, auditID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EvidencePack{}
	for rows.Next() {
		var p EvidencePack
		if err := rows.Scan(&p.ID, &p.AuditID, &p.PeriodStart, &p.PeriodEnd,
			&p.Pack, &p.PackSHA256, &p.AssembledBy, &p.AssembledAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

const certCols = `id, audit_id, evidence_pack_id, period_start, period_end,
	statement, signatories, pack_sha256, issued_by, approved_by,
	published_until, status, created_at`

func scanCert(row pgx.Row) (*SegregationCertification, error) {
	var c SegregationCertification
	if err := row.Scan(&c.ID, &c.AuditID, &c.EvidencePackID, &c.PeriodStart,
		&c.PeriodEnd, &c.Statement, &c.Signatories, &c.PackSHA256, &c.IssuedBy,
		&c.ApprovedBy, &c.PublishedUntil, &c.Status, &c.CreatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *PgStore) InsertCertification(ctx context.Context, c SegregationCertification) (*SegregationCertification, error) {
	return scanCert(s.q.QueryRow(ctx, `
		INSERT INTO segregation_certifications
		 (audit_id, evidence_pack_id, period_start, period_end, statement,
		  signatories, pack_sha256, issued_by, approved_by, published_until, status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+certCols,
		c.AuditID, c.EvidencePackID, c.PeriodStart, c.PeriodEnd, c.Statement,
		c.Signatories, c.PackSHA256, c.IssuedBy, c.ApprovedBy, c.PublishedUntil,
		c.Status))
}

func (s *PgStore) UpdateCertification(ctx context.Context, c SegregationCertification) error {
	_, err := s.q.Exec(ctx,
		`UPDATE segregation_certifications SET status=$2 WHERE id=$1`, c.ID, c.Status)
	return err
}

func (s *PgStore) ListCertifications(ctx context.Context) ([]SegregationCertification, error) {
	rows, err := s.q.Query(ctx,
		`SELECT `+certCols+` FROM segregation_certifications ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SegregationCertification{}
	for rows.Next() {
		var c SegregationCertification
		if err := rows.Scan(&c.ID, &c.AuditID, &c.EvidencePackID, &c.PeriodStart,
			&c.PeriodEnd, &c.Statement, &c.Signatories, &c.PackSHA256, &c.IssuedBy,
			&c.ApprovedBy, &c.PublishedUntil, &c.Status, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CertificationsCovering returns certifications whose attested range
// includes day — AssertReleaseGate picks the valid one.
func (s *PgStore) CertificationsCovering(ctx context.Context, day time.Time) ([]SegregationCertification, error) {
	rows, err := s.q.Query(ctx,
		`SELECT `+certCols+` FROM segregation_certifications
		  WHERE period_start <= $1 AND period_end >= $1 AND status='ISSUED'
		  ORDER BY id DESC`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SegregationCertification{}
	for rows.Next() {
		var c SegregationCertification
		if err := rows.Scan(&c.ID, &c.AuditID, &c.EvidencePackID, &c.PeriodStart,
			&c.PeriodEnd, &c.Statement, &c.Signatories, &c.PackSHA256, &c.IssuedBy,
			&c.ApprovedBy, &c.PublishedUntil, &c.Status, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
