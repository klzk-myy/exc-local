// Phase-21 Task 21.3.12 — MiFID II RTS 6: algorithm certification,
// Direct Electronic Access (DEA) controls and order-record retention
// (spec §14.9.1; RTS 6 Art. 9–17).
//
// Components:
//
//	algo_certifications    — Art. 9: an algo may submit only while a
//	                         CERTIFIED, unexpired registration exists
//	                         (test evidence + kill-button test +
//	                         capacity self-assessment recorded). The
//	                         orders admission seam (AlgoCertGate)
//	                         rejects uncertified algo flow with
//	                         ALGO_NOT_CERTIFIED (422).
//	dea_session_controls   — Art. 15: DEA sessions carry hard
//	                         pre-trade limits + sponsor attribution;
//	                         DEALimitsFor feeds session admission and
//	                         AssertDEAOrder gates oversized entries.
//	rts6_self_assessments  — the annual self-assessment register;
//	                         SweepAssessmentDue pages the officer queue
//	                         60d ahead of a missed deadline.
//	Order retention        — RTS 6 Art. 17 keeps order lifecycle
//	                         records ≥5y; retention is pinned by the
//	                         Phase-09 policy (order_audit: retain 1825d)
//	                         and ExportOrderLifecycle serves regulator
//	                         requests over that audit spine.
package compliance

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"

	excerrors "exchange/pkg/errors"
)

// RTS 6 cadence constants.
const (
	// AlgoCertValidity is the certification lifetime — annual
	// re-certification per RTS 6 Art. 9 review discipline.
	AlgoCertValidity = 365 * 24 * time.Hour
	// RTS6AssessWarnAhead is the self-assessment reminder horizon.
	RTS6AssessWarnAhead = 60 * 24 * time.Hour
)

// ErrAlgoNotCertified → ALGO_NOT_CERTIFIED (422, pre-registered).
var ErrAlgoNotCertified = excerrors.New("ALGO_NOT_CERTIFIED",
	"client algo strategy lacks a current RTS 6 certification")

// ---------------------------------------------------------------------------
// Algorithm certification register
// ---------------------------------------------------------------------------

// AlgoCertification is one algo_certifications row.
type AlgoCertification struct {
	ID                int64      `json:"id"`
	AlgoID            string     `json:"algo_id"`
	AccountID         int64      `json:"account_id"`
	Status            string     `json:"status"` // PENDING|CERTIFIED|SUSPENDED|EXPIRED|REVOKED
	TestEvidenceRef   string     `json:"test_evidence_ref"`
	KillButtonTested  bool       `json:"kill_button_tested"`
	CapacityAssessRef string     `json:"capacity_assessment_ref"`
	CertifiedBy       *int64     `json:"certified_by,omitempty"`
	CertifiedAt       *time.Time `json:"certified_at,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	ReviewDueAt       *time.Time `json:"review_due_at,omitempty"`
	CreatedBy         int64      `json:"created_by"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// CertifyRequest registers or renews a certification. Art. 9 evidence
// is mandatory — an untested algo can never reach CERTIFIED.
type CertifyRequest struct {
	AlgoID            string
	AccountID         int64
	TestEvidenceRef   string // conformance-testnet evidence ref
	KillButtonTested  bool   // kill-functionality test passed
	CapacityAssessRef string // Art. 9 capacity self-assessment ref
	ActorID           int64  // Compliance Officer
}

// DEAControls is one dea_session_controls row.
type DEAControls struct {
	ID             int64     `json:"id"`
	SessionID      string    `json:"session_id"`
	AccountID      int64     `json:"account_id"`
	MaxOrderQty    string    `json:"max_order_qty"` // decimal string
	MaxMsgsPerSec  int       `json:"max_msgs_per_sec"`
	SponsoringDesk string    `json:"sponsoring_desk"`
	DropCopyFeed   string    `json:"drop_copy_feed"`
	Status         string    `json:"status"`
	CreatedBy      int64     `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// RTS6Assessment is one rts6_self_assessments row.
type RTS6Assessment struct {
	ID          int64      `json:"id"`
	PeriodYear  int        `json:"period_year"`
	DocumentRef string     `json:"document_ref"`
	Status      string     `json:"status"` // DRAFT|SUBMITTED|REVIEWED
	FiledBy     int64      `json:"filed_by"`
	FiledAt     *time.Time `json:"filed_at,omitempty"`
	ReviewedBy  *int64     `json:"reviewed_by,omitempty"`
	ReviewedAt  *time.Time `json:"reviewed_at,omitempty"`
	ReviewNote  string     `json:"review_note"`
	DueAt       time.Time  `json:"due_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

// RTS6Alerter pages the officer queue (assessment-due / cert-expiry
// warnings). HoldAlerter-compatible shape — the gateway adapts the ops
// alert sink.
type RTS6Alerter interface {
	RaiseHold(ctx context.Context, a HoldAlert) error
}

// RTS6Service owns the RTS 6 registers. Role gates follow the
// Compliance Officer convention; reads stay open to the route auth.
type RTS6Service struct {
	pool     *pgxpool.Pool
	resolver HoldRoleResolver
	alerter  RTS6Alerter
	now      func() time.Time
}

// NewRTS6Service binds the register; resolver is the admin role
// resolver, alerter may be nil (sweeps then only flip state).
func NewRTS6Service(pool *pgxpool.Pool, resolver HoldRoleResolver,
	alerter RTS6Alerter) *RTS6Service {
	return &RTS6Service{pool: pool, resolver: resolver,
		alerter: alerter, now: time.Now}
}

// WithClock overrides the clock (tests).
func (s *RTS6Service) WithClock(f func() time.Time) *RTS6Service {
	if f != nil {
		s.now = f
	}
	return s
}

func (s *RTS6Service) checkRole(ctx context.Context, userID int64) error {
	if s.resolver == nil {
		return excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	role, err := s.resolver(ctx, userID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if role != "Compliance Officer" && role != "Super Admin" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot administer RTS 6 controls")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Certifications
// ---------------------------------------------------------------------------

// Certify registers (or renews) an algo certification. Art. 9(2):
// certification demands conformance-testnet evidence, a passed
// kill-functionality test and the capacity self-assessment ref.
// Idempotent upsert on (account_id, algo_id) — renewal resets the
// expiry clock; a REVOKED cert can only return via a new Certify with
// fresh evidence.
func (s *RTS6Service) Certify(ctx context.Context,
	req CertifyRequest) (*AlgoCertification, error) {
	if req.AlgoID == "" || req.AccountID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"algo_id and account_id are required")
	}
	if err := s.checkRole(ctx, req.ActorID); err != nil {
		return nil, err
	}
	if req.TestEvidenceRef == "" || req.CapacityAssessRef == "" ||
		!req.KillButtonTested {
		return nil, excerrors.New("INVALID_REQUEST",
			"RTS 6 Art. 9 certification requires testnet evidence, a "+
				"passed kill-button test and a capacity self-assessment ref")
	}
	now := s.now().UTC()
	expires := now.Add(AlgoCertValidity)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "certify tx: "+err.Error())
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var c AlgoCertification
	err = tx.QueryRow(ctx, `
		INSERT INTO algo_certifications
		    (algo_id, account_id, status, test_evidence_ref,
		     kill_button_tested, capacity_assessment_ref,
		     certified_by, certified_at, expires_at, created_by)
		VALUES ($1,$2,'CERTIFIED',$3,$4,$5,$6,$7,$8,$6)
		ON CONFLICT (account_id, algo_id) DO UPDATE SET
		    status='CERTIFIED', test_evidence_ref=$3,
		    kill_button_tested=$4, capacity_assessment_ref=$5,
		    certified_by=$6, certified_at=$7, expires_at=$8,
		    updated_at=now()
		RETURNING id, algo_id, account_id, status, test_evidence_ref,
		          kill_button_tested, capacity_assessment_ref,
		          certified_by, certified_at, expires_at, review_due_at,
		          created_by, created_at, updated_at`,
		req.AlgoID, req.AccountID, req.TestEvidenceRef,
		req.KillButtonTested, req.CapacityAssessRef,
		req.ActorID, now, expires).
		Scan(&c.ID, &c.AlgoID, &c.AccountID, &c.Status,
			&c.TestEvidenceRef, &c.KillButtonTested, &c.CapacityAssessRef,
			&c.CertifiedBy, &c.CertifiedAt, &c.ExpiresAt, &c.ReviewDueAt,
			&c.CreatedBy, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "certify: "+err.Error())
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: req.ActorID,
		Action:      "rts6.algo_certify",
		TargetType:  "algo_certification",
		TargetID:    &c.ID,
		AfterState:  c,
	}); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "certify audit: "+err.Error())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "certify commit: "+err.Error())
	}
	return &c, nil
}

// AssertCertified is the orders.AlgoCertGate: nil ⇔ a CERTIFIED,
// unexpired registration exists for (account, algo). Fail-closed —
// a store error rejects as INTERNAL_ERROR rather than admitting.
func (s *RTS6Service) AssertCertified(ctx context.Context,
	accountID int64, algoID string) error {
	var status string
	var expires *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT status, expires_at FROM algo_certifications
		 WHERE account_id = $1 AND algo_id = $2`,
		accountID, algoID).Scan(&status, &expires)
	if err == pgx.ErrNoRows {
		return ErrAlgoNotCertified
	}
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "cert lookup: "+err.Error())
	}
	if status != "CERTIFIED" ||
		(expires != nil && !expires.After(s.now().UTC())) {
		return ErrAlgoNotCertified
	}
	return nil
}

// Transition flips a certification's status (SUSPEND is reversible,
// REVOKE terminal). Reason lands in the audit row.
func (s *RTS6Service) Transition(ctx context.Context, actorID int64,
	certID int64, to, reason string) (*AlgoCertification, error) {
	if to != "SUSPENDED" && to != "REVOKED" && to != "CERTIFIED" {
		return nil, excerrors.New("INVALID_REQUEST",
			"transition must be SUSPENDED|CERTIFIED|REVOKED")
	}
	if err := s.checkRole(ctx, actorID); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "cert tx: "+err.Error())
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var before string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM algo_certifications WHERE id = $1 FOR UPDATE`,
		certID).Scan(&before); err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "certification not found")
	} else if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "cert lock: "+err.Error())
	}
	if before == "REVOKED" {
		return nil, excerrors.New("INVALID_REQUEST",
			"a revoked certification is terminal — re-certify with fresh evidence")
	}
	var c AlgoCertification
	err = tx.QueryRow(ctx, `
		UPDATE algo_certifications SET status = $2, updated_at = now()
		 WHERE id = $1
		RETURNING id, algo_id, account_id, status, test_evidence_ref,
		          kill_button_tested, capacity_assessment_ref,
		          certified_by, certified_at, expires_at, review_due_at,
		          created_by, created_at, updated_at`,
		certID, to).
		Scan(&c.ID, &c.AlgoID, &c.AccountID, &c.Status,
			&c.TestEvidenceRef, &c.KillButtonTested, &c.CapacityAssessRef,
			&c.CertifiedBy, &c.CertifiedAt, &c.ExpiresAt, &c.ReviewDueAt,
			&c.CreatedBy, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "cert update: "+err.Error())
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actorID,
		Action:      "rts6.algo_" + to,
		TargetType:  "algo_certification",
		TargetID:    &certID,
		BeforeState: map[string]any{"status": before},
		AfterState:  map[string]any{"status": to, "reason": reason},
	}); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "cert audit: "+err.Error())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "cert commit: "+err.Error())
	}
	return &c, nil
}

// ListCertifications reads the register (dashboard), newest first.
func (s *RTS6Service) ListCertifications(ctx context.Context,
	status string, limit int) ([]AlgoCertification, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, algo_id, account_id, status, test_evidence_ref,
	             kill_button_tested, capacity_assessment_ref,
	             certified_by, certified_at, expires_at, review_due_at,
	             created_by, created_at, updated_at
	        FROM algo_certifications`
	args := []any{}
	if status != "" {
		q += " WHERE status = $1"
		args = append(args, status)
	}
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT %d", limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "cert list: "+err.Error())
	}
	defer rows.Close()
	var out []AlgoCertification
	for rows.Next() {
		var c AlgoCertification
		if err := rows.Scan(&c.ID, &c.AlgoID, &c.AccountID, &c.Status,
			&c.TestEvidenceRef, &c.KillButtonTested, &c.CapacityAssessRef,
			&c.CertifiedBy, &c.CertifiedAt, &c.ExpiresAt, &c.ReviewDueAt,
			&c.CreatedBy, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR", "cert row: "+err.Error())
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SweepCertExpiry flips CERTIFIED rows past expiry to EXPIRED and
// pages the officer queue for certs expiring inside RTS6AssessWarnAhead.
// Returns expired-flip count.
func (s *RTS6Service) SweepCertExpiry(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE algo_certifications SET status='EXPIRED', updated_at=now()
		 WHERE status='CERTIFIED' AND expires_at IS NOT NULL
		   AND expires_at <= now()`)
	if err != nil {
		return 0, excerrors.New("INTERNAL_ERROR", "cert expiry: "+err.Error())
	}
	if s.alerter != nil {
		var due int
		_ = s.pool.QueryRow(ctx, `
			SELECT count(*) FROM algo_certifications
			 WHERE status='CERTIFIED' AND expires_at IS NOT NULL
			   AND expires_at <= now() + $1::interval`,
			RTS6AssessWarnAhead.String()).Scan(&due)
		if due > 0 {
			_ = s.alerter.RaiseHold(ctx, HoldAlert{
				Severity: "P2", Code: "RTS6_CERT_EXPIRY",
				Summary: fmt.Sprintf(
					"%d algo certification(s) expire within %dd", due, 60),
			})
		}
	}
	return int(tag.RowsAffected()), nil
}

// ---------------------------------------------------------------------------
// DEA session controls
// ---------------------------------------------------------------------------

// SetDEALimits upserts the DEA session control row (Compliance
// Officer). The sponsoring desk + drop-copy feed bind the Art. 15
// sponsorship contract; limits are hard pre-trade gates.
func (s *RTS6Service) SetDEALimits(ctx context.Context,
	c DEAControls) (*DEAControls, error) {
	if c.SessionID == "" || c.AccountID <= 0 || c.MaxOrderQty == "" ||
		c.MaxMsgsPerSec <= 0 || c.SponsoringDesk == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"DEA controls require session_id, account_id, "+
				"max_order_qty, max_msgs_per_sec and sponsoring_desk")
	}
	if err := s.checkRole(ctx, c.CreatedBy); err != nil {
		return nil, err
	}
	var out DEAControls
	err := s.pool.QueryRow(ctx, `
		INSERT INTO dea_session_controls
		    (session_id, account_id, max_order_qty, max_msgs_per_sec,
		     sponsoring_desk, drop_copy_feed, status, created_by)
		VALUES ($1,$2,$3::decimal,$4,$5,$6,'ACTIVE',$7)
		ON CONFLICT (session_id) DO UPDATE SET
		    account_id=$2, max_order_qty=$3::decimal,
		    max_msgs_per_sec=$4, sponsoring_desk=$5,
		    drop_copy_feed=$6, updated_at=now()
		RETURNING id, session_id, account_id, max_order_qty::text,
		          max_msgs_per_sec, sponsoring_desk, drop_copy_feed,
		          status, created_by, created_at, updated_at`,
		c.SessionID, c.AccountID, c.MaxOrderQty, c.MaxMsgsPerSec,
		c.SponsoringDesk, c.DropCopyFeed, c.CreatedBy).
		Scan(&out.ID, &out.SessionID, &out.AccountID, &out.MaxOrderQty,
			&out.MaxMsgsPerSec, &out.SponsoringDesk, &out.DropCopyFeed,
			&out.Status, &out.CreatedBy, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "dea upsert: "+err.Error())
	}
	_, _, _ = admin.LogAuto(ctx, s.pool, admin.AuditEntry{
		AdminUserID: c.CreatedBy, Action: "rts6.dea_set",
		TargetType: "dea_session_control", TargetID: &out.ID,
		AfterState: out,
	})
	return &out, nil
}

// DEALimitsFor resolves a session's controls — the FIX/SBE session
// admission reads this before accepting entry (false = unregistered;
// DEA sessions without a control row must fail closed upstream).
func (s *RTS6Service) DEALimitsFor(ctx context.Context,
	sessionID string) (*DEAControls, bool, error) {
	var c DEAControls
	err := s.pool.QueryRow(ctx, `
		SELECT id, session_id, account_id, max_order_qty::text,
		       max_msgs_per_sec, sponsoring_desk, drop_copy_feed,
		       status, created_by, created_at, updated_at
		  FROM dea_session_controls WHERE session_id = $1`,
		sessionID).
		Scan(&c.ID, &c.SessionID, &c.AccountID, &c.MaxOrderQty,
			&c.MaxMsgsPerSec, &c.SponsoringDesk, &c.DropCopyFeed,
			&c.Status, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, excerrors.New("INTERNAL_ERROR",
			"dea lookup: "+err.Error())
	}
	return &c, true, nil
}

// SuspendDEA flips a session's control row to SUSPENDED — the FIX
// seam then refuses entry until a fresh SetDEALimits.
func (s *RTS6Service) SuspendDEA(ctx context.Context, actorID int64,
	sessionID, reason string) error {
	if err := s.checkRole(ctx, actorID); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE dea_session_controls SET status='SUSPENDED', updated_at=now()
		 WHERE session_id = $1 AND status='ACTIVE'`, sessionID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "dea suspend: "+err.Error())
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New("NOT_FOUND", "no active DEA control for session")
	}
	_, _, _ = admin.LogAuto(ctx, s.pool, admin.AuditEntry{
		AdminUserID: actorID, Action: "rts6.dea_suspend",
		TargetType: "dea_session_control",
		AfterState: map[string]any{"session_id": sessionID, "reason": reason},
	})
	return nil
}

// ---------------------------------------------------------------------------
// Annual self-assessment
// ---------------------------------------------------------------------------

// FileAssessment files (or files over) the year's self-assessment —
// DRAFT → SUBMITTED. due_at anchors the annual deadline the sweep
// watches.
func (s *RTS6Service) FileAssessment(ctx context.Context, year int,
	docRef string, filedBy int64, dueAt time.Time) (*RTS6Assessment, error) {
	if year < 2000 || docRef == "" || dueAt.IsZero() {
		return nil, excerrors.New("INVALID_REQUEST",
			"assessment requires period_year, document_ref and due_at")
	}
	if err := s.checkRole(ctx, filedBy); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	var a RTS6Assessment
	err := s.pool.QueryRow(ctx, `
		INSERT INTO rts6_self_assessments
		    (period_year, document_ref, status, filed_by, filed_at, due_at)
		VALUES ($1,$2,'SUBMITTED',$3,$4,$5)
		ON CONFLICT (period_year) DO UPDATE SET
		    document_ref=$2, status='SUBMITTED', filed_by=$3,
		    filed_at=$4, due_at=$5, updated_at=now()
		RETURNING id, period_year, document_ref, status, filed_by,
		          filed_at, reviewed_by, reviewed_at, review_note,
		          due_at, created_at`,
		year, docRef, filedBy, now, dueAt.UTC()).
		Scan(&a.ID, &a.PeriodYear, &a.DocumentRef, &a.Status, &a.FiledBy,
			&a.FiledAt, &a.ReviewedBy, &a.ReviewedAt, &a.ReviewNote,
			&a.DueAt, &a.CreatedAt)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "assessment file: "+err.Error())
	}
	_, _, _ = admin.LogAuto(ctx, s.pool, admin.AuditEntry{
		AdminUserID: filedBy, Action: "rts6.assessment_file",
		TargetType: "rts6_self_assessment", TargetID: &a.ID,
		AfterState: a,
	})
	return &a, nil
}

// ReviewAssessment records the second-officer review of a SUBMITTED
// assessment — distinct reviewer from filer (separation of duties).
func (s *RTS6Service) ReviewAssessment(ctx context.Context, reviewerID,
	assessmentID int64, note string) (*RTS6Assessment, error) {
	if err := s.checkRole(ctx, reviewerID); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "review tx: "+err.Error())
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var filedBy int64
	var status string
	if err := tx.QueryRow(ctx,
		`SELECT filed_by, status FROM rts6_self_assessments
		  WHERE id = $1 FOR UPDATE`, assessmentID).
		Scan(&filedBy, &status); err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "assessment not found")
	} else if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "assessment lock: "+err.Error())
	}
	if status != "SUBMITTED" {
		return nil, excerrors.New("INVALID_REQUEST",
			"assessment is "+status+" — only SUBMITTED rows review")
	}
	if filedBy == reviewerID {
		return nil, excerrors.New("DUAL_CONTROL_REQUIRED",
			"assessment review requires a different officer than the filer")
	}
	var a RTS6Assessment
	err = tx.QueryRow(ctx, `
		UPDATE rts6_self_assessments
		   SET status='REVIEWED', reviewed_by=$2, reviewed_at=$3,
		       review_note=$4, updated_at=now()
		 WHERE id = $1
		RETURNING id, period_year, document_ref, status, filed_by,
		          filed_at, reviewed_by, reviewed_at, review_note,
		          due_at, created_at`,
		assessmentID, reviewerID, s.now().UTC(), note).
		Scan(&a.ID, &a.PeriodYear, &a.DocumentRef, &a.Status, &a.FiledBy,
			&a.FiledAt, &a.ReviewedBy, &a.ReviewedAt, &a.ReviewNote,
			&a.DueAt, &a.CreatedAt)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "assessment review: "+err.Error())
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: reviewerID, Action: "rts6.assessment_review",
		TargetType: "rts6_self_assessment", TargetID: &a.ID,
		AfterState: a,
	}); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "assessment audit: "+err.Error())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "assessment commit: "+err.Error())
	}
	return &a, nil
}

// ListAssessments reads the register, newest period first.
func (s *RTS6Service) ListAssessments(ctx context.Context,
	limit int) ([]RTS6Assessment, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, period_year, document_ref, status, filed_by,
		       filed_at, reviewed_by, reviewed_at, review_note,
		       due_at, created_at
		  FROM rts6_self_assessments
		 ORDER BY period_year DESC LIMIT $1`, limit)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "assessment list: "+err.Error())
	}
	defer rows.Close()
	var out []RTS6Assessment
	for rows.Next() {
		var a RTS6Assessment
		if err := rows.Scan(&a.ID, &a.PeriodYear, &a.DocumentRef,
			&a.Status, &a.FiledBy, &a.FiledAt, &a.ReviewedBy,
			&a.ReviewedAt, &a.ReviewNote, &a.DueAt, &a.CreatedAt); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"assessment row: "+err.Error())
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SweepAssessmentDue pages the officer queue when the current year's
// assessment is missing or the next due_at approaches unfiled —
// the "must be reviewed annually" enforcement (RTS 6 Art. 9).
func (s *RTS6Service) SweepAssessmentDue(ctx context.Context) (int, error) {
	year := s.now().UTC().Year()
	var status string
	var dueAt time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT status, due_at FROM rts6_self_assessments
		 WHERE period_year = $1`, year).Scan(&status, &dueAt)
	if err == pgx.ErrNoRows {
		if s.alerter != nil {
			_ = s.alerter.RaiseHold(ctx, HoldAlert{
				Severity: "P2", Code: "RTS6_ASSESSMENT_DUE",
				Summary: fmt.Sprintf(
					"no RTS 6 self-assessment filed for %d", year),
			})
		}
		return 1, nil
	}
	if err != nil {
		return 0, excerrors.New("INTERNAL_ERROR",
			"assessment sweep: "+err.Error())
	}
	if status != "REVIEWED" &&
		dueAt.Sub(s.now().UTC()) <= RTS6AssessWarnAhead && s.alerter != nil {
		_ = s.alerter.RaiseHold(ctx, HoldAlert{
			Severity: "P2", Code: "RTS6_ASSESSMENT_DUE",
			Summary: fmt.Sprintf(
				"RTS 6 %d self-assessment is %s, due %s",
				year, status, dueAt.Format("2006-01-02")),
		})
		return 1, nil
	}
	return 0, nil
}

// ---------------------------------------------------------------------------
// Order-record retention export (RTS 6 Art. 17 — ≥5y)
// ---------------------------------------------------------------------------

// OrderLifecycleRow is one order_audit row in the export payload
// (schema pinned by migration 153 — field-level change journal).
type OrderLifecycleRow struct {
	AuditID    int64     `json:"audit_id"`
	OrderID    int64     `json:"order_id"`
	AccountID  int64     `json:"account_id"`
	Operation  string    `json:"operation"`
	FieldName  string    `json:"field_name"`
	OldValue   *string   `json:"old_value,omitempty"`
	NewValue   *string   `json:"new_value,omitempty"`
	ModifiedBy string    `json:"modified_by"`
	RequestID  *string   `json:"request_id,omitempty"`
	ModifiedAt time.Time `json:"modified_at"`
	IPAddress  *string   `json:"ip_address,omitempty"`
}

// ExportOrderLifecycle serves the regulator-requested order record —
// the full lifecycle trail from order_audit (retention pinned 1825d
// by the Phase-09 policy entry; rows are append-only).
func (s *RTS6Service) ExportOrderLifecycle(ctx context.Context,
	orderID int64) ([]OrderLifecycleRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT audit_id, order_id, account_id, operation, field_name,
		       old_value, new_value, modified_by, request_id,
		       modified_at, ip_address
		  FROM order_audit
		 WHERE order_id = $1
		 ORDER BY audit_id ASC`, orderID)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"order lifecycle export: "+err.Error())
	}
	defer rows.Close()
	var out []OrderLifecycleRow
	for rows.Next() {
		var r OrderLifecycleRow
		if err := rows.Scan(&r.AuditID, &r.OrderID, &r.AccountID,
			&r.Operation, &r.FieldName, &r.OldValue, &r.NewValue,
			&r.ModifiedBy, &r.RequestID, &r.ModifiedAt,
			&r.IPAddress); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"lifecycle row: "+err.Error())
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
