// Order execution policy publication, consent & annual review —
// Phase-21 Task 21.3.28 (spec §14.13/§5.42.2; §24 #377; remediation
// #30: "no new codes — existing PRODUCT_NOT_PERMITTED + product-gating
// path cover rejections").
//
//   - execution_policies (migration 100): DRAFT → ACTIVE → SUPERSEDED,
//     exactly one ACTIVE row. The public GET /api/v1/execution-policy
//     surface serves it; the ACTIVE row is immutable except the review
//     stamp (DB trigger — backdated corrections land as a NEW version,
//     never a mutation).
//   - Consent is version-scoped: CheckConsent gates order admission on
//     the CURRENT ACTIVE version through the Task 14.3.7 product-gating
//     path — a missing consent rejects opens with
//     PRODUCT_NOT_PERMITTED (close-only posture; reduce_only bypasses
//     like every exposure-increasing gate). Refusal = no consent row →
//     naturally close-only.
//   - Consent writes both ledgers atomically: execution_policy_consents
//     (policy-scoped) + account_consents EXECUTION_POLICY (the generic
//     gate table, migration 239) in one transaction.
//   - Annual review: Review() snapshots the assembled evidence pack
//     (never mutates live evidence), stamps the CCO sign-off and rolls
//     review_due_at forward ≤12 months. An overdue review pages a
//     Compliance P1 (ops alerter) and freezes version upgrades —
//     Activate() refuses while the ACTIVE review is overdue; the
//     ACTIVE version stays enforceable meanwhile.
//   - Material amendments reference the regulatory-change record they
//     flow through (Task 21.3.25) — Activate stores the link inside
//     evidence_pack.activation so impact mapping stays traceable.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"
	excerrors "exchange/pkg/errors"
)

// Execution policy statuses (migration 100 CHECK mirror).
const (
	PolicyDraft      = "DRAFT"
	PolicyActive     = "ACTIVE"
	PolicySuperseded = "SUPERSEDED"
)

// ConsentExecPolicy is the account_consents consent_type for the venue
// execution policy (migration 239 CHECK mirror).
const ConsentExecPolicy = "EXECUTION_POLICY"

// PolicyReviewOverdueCode is the ops-alert code for an overdue annual
// review — an alert code, not a wire code (deliberately unregistered).
const PolicyReviewOverdueCode = "EXECUTION_POLICY_REVIEW_OVERDUE"

// PolicyMaxReviewHorizon is the §14.13 annual-review bound.
const PolicyMaxReviewHorizon = 365 * 24 * time.Hour

// ExecutionPolicy is one execution_policies row.
type ExecutionPolicy struct {
	ID             int64           `json:"id"`
	Version        string          `json:"version"`
	BodyRef        string          `json:"body_ref"`
	Status         string          `json:"status"`
	EffectiveFrom  *time.Time      `json:"effective_from,omitempty"`
	ReviewDueAt    *time.Time      `json:"review_due_at,omitempty"`
	MaterialChange bool            `json:"material_change"`
	Approver       *int64          `json:"approver,omitempty"`
	ApprovedAt     *time.Time      `json:"approved_at,omitempty"`
	EvidencePack   json.RawMessage `json:"evidence_pack"`
	ReviewNotes    string          `json:"review_notes,omitempty"`
	ReviewedBy     *int64          `json:"reviewed_by,omitempty"`
	ReviewedAt     *time.Time      `json:"reviewed_at,omitempty"`
	CreatedBy      int64           `json:"created_by"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	// Code carries EXECUTION_POLICY_REVIEW_OVERDUE while the ACTIVE
	// review deadline has lapsed (alert code on the payload).
	Code string `json:"code,omitempty"`
}

// PolicyConsent is one execution_policy_consents row.
type PolicyConsent struct {
	ID          int64     `json:"id"`
	AccountID   int64     `json:"account_id"`
	PolicyID    int64     `json:"policy_id"`
	Version     string    `json:"version"`
	ConsentedAt time.Time `json:"consented_at"`
	ConsentedBy *int64    `json:"consented_by,omitempty"`
	IP          string    `json:"ip,omitempty"`
}

// PolicyEvidenceSource assembles the annual-review evidence pack
// (RTS 27/28 outputs, TCA summaries, incident-linked mis-executions)
// — the gateway wires adapters over the reporting stores; nil falls
// back to the built-in trade-bust probe so a review never fabricates
// "no incidents" silently (the pack records which sources were absent).
type PolicyEvidenceSource interface {
	AssemblePolicyEvidence(ctx context.Context, policyID int64) (map[string]any, error)
}

// ExecutionPolicyService owns the lifecycle + the consent gate.
type ExecutionPolicyService struct {
	pool     *pgxpool.Pool
	resolver HoldRoleResolver
	alerter  HoldAlerter
	evidence PolicyEvidenceSource
	now      func() time.Time
}

// NewExecutionPolicyService wires the service; pool + resolver required.
func NewExecutionPolicyService(pool *pgxpool.Pool,
	resolver HoldRoleResolver) (*ExecutionPolicyService, error) {
	if pool == nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"execution policy service requires pool")
	}
	if resolver == nil {
		return nil, excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	return &ExecutionPolicyService{pool: pool, resolver: resolver,
		now: time.Now}, nil
}

// WithAlerter wires the ops channel for the overdue-review P1.
func (s *ExecutionPolicyService) WithAlerter(a HoldAlerter) *ExecutionPolicyService {
	s.alerter = a
	return s
}

// WithEvidenceSource wires the RTS27/28 + TCA evidence assembler.
func (s *ExecutionPolicyService) WithEvidenceSource(e PolicyEvidenceSource) *ExecutionPolicyService {
	s.evidence = e
	return s
}

// WithClock overrides the clock (tests).
func (s *ExecutionPolicyService) WithClock(c func() time.Time) *ExecutionPolicyService {
	s.now = c
	return s
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// CreateDraft files a new DRAFT version — the only way to introduce a
// policy body (an ACTIVE row is never patched; corrections land as a
// new version).
func (s *ExecutionPolicyService) CreateDraft(ctx context.Context, version,
	bodyRef string, officer int64) (*ExecutionPolicy, error) {
	if err := s.checkRole(ctx, officer); err != nil {
		return nil, err
	}
	if version == "" || bodyRef == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"version and body_ref are required")
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO execution_policies (version, body_ref, created_by)
		VALUES ($1,$2,$3) RETURNING id`, version, bodyRef, officer).Scan(&id)
	if err != nil {
		return nil, excerrors.Wrap("INVALID_REQUEST",
			"execution policy version exists or insert failed", err)
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "execution_policies",
		&id, "POLICY_DRAFT", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "policy audit", err)
	}
	return s.get(ctx, id)
}

// Activate approves a DRAFT (CCO sign-off): it becomes the publicly
// served ACTIVE version; any prior ACTIVE transitions to SUPERSEDED in
// the same transaction. Frozen while the incumbent's annual review is
// overdue (the ACTIVE version stays enforceable — CheckConsent keeps
// admitting on it).
func (s *ExecutionPolicyService) Activate(ctx context.Context, policyID,
	approver int64, effectiveFrom, reviewDueAt time.Time,
	materialChange bool, regChangeID *int64) (*ExecutionPolicy, error) {
	if err := s.checkRole(ctx, approver); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	if effectiveFrom.IsZero() {
		effectiveFrom = now
	}
	if reviewDueAt.IsZero() {
		return nil, excerrors.New("INVALID_REQUEST",
			"review_due_at is required — the annual review deadline is mandatory")
	}
	if reviewDueAt.Before(effectiveFrom) {
		return nil, excerrors.New("INVALID_REQUEST",
			"review_due_at precedes effective_from")
	}
	if reviewDueAt.Sub(effectiveFrom) > PolicyMaxReviewHorizon {
		return nil, excerrors.New("INVALID_REQUEST",
			"review_due_at exceeds the 12-month review horizon")
	}
	// Amendment link: a material change superseding an incumbent must
	// name the regulatory-change record it flows through (§14.13
	// change control) — the first-ever activation is exempt (nothing
	// supersedes).
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "policy activate tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	err = tx.QueryRow(ctx, `
		SELECT status FROM execution_policies WHERE id=$1 FOR UPDATE`,
		policyID).Scan(&status)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "execution policy not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "policy lock", err)
	}
	if status != PolicyDraft {
		return nil, excerrors.New("INVALID_REQUEST",
			"policy is "+status+" — only a DRAFT can activate")
	}

	var incumbentID *int64
	var incumbentDue *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT id, review_due_at FROM execution_policies
		WHERE status='ACTIVE' FOR UPDATE`).
		Scan(&incumbentID, &incumbentDue); err != nil && err != pgx.ErrNoRows {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "incumbent probe", err)
	}
	if incumbentID != nil {
		if incumbentDue != nil && incumbentDue.Before(now) {
			return nil, excerrors.New("INVALID_REQUEST",
				"annual review on the ACTIVE policy is overdue — "+
					"policy-version upgrades are frozen until the CCO review completes")
		}
		if materialChange && regChangeID == nil {
			return nil, excerrors.New("INVALID_REQUEST",
				"material amendments must reference the regulatory-change "+
					"record they flow through (Task 21.3.25)")
		}
	}
	if regChangeID != nil {
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM regulatory_changes WHERE change_id=$1)`,
			*regChangeID).Scan(&exists); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "reg-change probe", err)
		}
		if !exists {
			return nil, excerrors.New("INVALID_REQUEST",
				"reg_change_id does not reference a watch-register record")
		}
	}
	if incumbentID != nil {
		if _, err := tx.Exec(ctx, `
			UPDATE execution_policies SET status='SUPERSEDED', updated_at=now()
			WHERE id=$1`, *incumbentID); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "incumbent supersede", err)
		}
	}
	activation := map[string]any{
		"activated_at": now.Format(time.RFC3339),
	}
	if materialChange {
		activation["material_change"] = true
	}
	if regChangeID != nil {
		activation["reg_change_id"] = *regChangeID
	}
	actRaw, _ := json.Marshal(map[string]any{"activation": activation})
	if _, err := tx.Exec(ctx, `
		UPDATE execution_policies
		SET status='ACTIVE', effective_from=$2, review_due_at=$3,
		    material_change=$4, approver=$5, approved_at=now(),
		    evidence_pack = evidence_pack || $6::jsonb, updated_at=now()
		WHERE id=$1`, policyID, effectiveFrom, reviewDueAt,
		materialChange, approver, actRaw); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "policy activate", err)
	}
	if _, err := audit.Append(ctx, tx, "execution_policies", &policyID,
		"POLICY_ACTIVE", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "policy audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "policy activate commit", err)
	}
	return s.get(ctx, policyID)
}

// Review is the annual CCO review: snapshots the assembled evidence
// pack, stamps the sign-off and rolls review_due_at forward (≤12
// months — the only sanctioned way to unfreeze upgrades). The ACTIVE
// row's substantive fields stay immutable; only the review stamp +
// merged evidence move.
func (s *ExecutionPolicyService) Review(ctx context.Context, policyID,
	officer int64, notes string, newReviewDueAt time.Time) (*ExecutionPolicy, error) {
	if err := s.checkRole(ctx, officer); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	if newReviewDueAt.IsZero() || newReviewDueAt.Sub(now) > PolicyMaxReviewHorizon {
		return nil, excerrors.New("INVALID_REQUEST",
			"new review_due_at must be within 12 months of the review")
	}
	p, err := s.get(ctx, policyID)
	if err != nil {
		return nil, err
	}
	if p.Status != PolicyActive {
		return nil, excerrors.New("INVALID_REQUEST",
			"only the ACTIVE policy undergoes annual review")
	}
	// Assemble the evidence pack — snapshot semantics: the seam's
	// output is recorded verbatim; missing sources are named, never
	// fabricated.
	pack := map[string]any{
		"reviewed_at": now.Format(time.RFC3339),
		"sources":     []string{"trade_busts"},
	}
	busts, berr := s.bustEvidence(ctx)
	if berr != nil {
		return nil, berr
	}
	pack["trade_busts"] = busts
	if s.evidence != nil {
		ext, eerr := s.evidence.AssemblePolicyEvidence(ctx, policyID)
		if eerr != nil {
			return nil, excerrors.Wrap("SERVICE_DEGRADED",
				"evidence assembly failed — review cannot proceed (fail closed)", eerr)
		}
		for k, v := range ext {
			pack[k] = v
		}
		if srcs, ok := ext["sources"].([]string); ok {
			pack["sources"] = append(pack["sources"].([]string), srcs...)
		} else {
			pack["sources"] = append(pack["sources"].([]string), "external")
		}
	}
	raw, _ := json.Marshal(map[string]any{"annual_review": pack})
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "policy review tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE execution_policies
		SET review_due_at=$2, reviewed_by=$3, reviewed_at=now(),
		    review_notes=NULLIF($4,''),
		    evidence_pack = evidence_pack || $5::jsonb, updated_at=now()
		WHERE id=$1 AND status='ACTIVE'`,
		policyID, newReviewDueAt, officer, notes, raw); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "policy review", err)
	}
	if _, err := audit.Append(ctx, tx, "execution_policies", &policyID,
		"POLICY_REVIEW", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "policy review audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "policy review commit", err)
	}
	return s.get(ctx, policyID)
}

// bustEvidence counts incident-linked mis-executions (trade busts,
// Task 15.3.5) for the evidence pack — split by status so the review
// sees open incidents too.
func (s *ExecutionPolicyService) bustEvidence(ctx context.Context) (map[string]any, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT status::text, count(*) FROM trade_busts GROUP BY status`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "bust evidence", err)
	}
	defer rows.Close()
	byStatus := map[string]int64{}
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "bust scan", err)
		}
		byStatus[st] = n
	}
	return map[string]any{"by_status": byStatus}, rows.Err()
}

// ---------------------------------------------------------------------------
// Consent — the order-admission gate
// ---------------------------------------------------------------------------

// CheckConsent is the Task 14.3.7 product-gating admission: nil admits;
// PRODUCT_NOT_PERMITTED rejects. reduce_only bypasses (close-only
// posture); no ACTIVE policy means nothing to consent to (pre-launch).
// A probe failure fails closed with SERVICE_DEGRADED.
func (s *ExecutionPolicyService) CheckConsent(ctx context.Context,
	accountID int64, reduceOnly bool) error {
	if reduceOnly {
		return nil
	}
	p, err := s.active(ctx)
	if err != nil {
		return err
	}
	if p == nil {
		return nil
	}
	var has bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM account_consents
			WHERE account_id=$1 AND consent_type=$2 AND doc_ref=$3)`,
		accountID, ConsentExecPolicy, p.Version).Scan(&has); err != nil {
		return excerrors.Wrap("SERVICE_DEGRADED",
			"execution-policy consent probe failed — order rejected (fail closed)", err)
	}
	if !has {
		return excerrors.New("PRODUCT_NOT_PERMITTED",
			"execution policy "+p.Version+" consent required — order entry "+
				"is close-only until consent is recorded")
	}
	return nil
}

// PolicyConsentGate composes the consent check with the inner
// product-profile gate — it satisfies the orders.ProductGate seam
// (AdmitOrder signature) without the compliance package importing
// orders: declare the inner seam by method shape.
type PolicyConsentGate struct {
	Inner interface {
		AdmitOrder(ctx context.Context, accountID int64,
			symbol, instrumentClass string, reduceOnly bool) error
	}
	Policy *ExecutionPolicyService
}

// AdmitOrder runs the inner profile/target-market gate, then the
// version-scoped consent check — a refusal reads PRODUCT_NOT_PERMITTED.
func (g *PolicyConsentGate) AdmitOrder(ctx context.Context, accountID int64,
	symbol, instrumentClass string, reduceOnly bool) error {
	if g.Inner != nil {
		if err := g.Inner.AdmitOrder(ctx, accountID, symbol,
			instrumentClass, reduceOnly); err != nil {
			return err
		}
	}
	if g.Policy == nil {
		return excerrors.New("SERVICE_DEGRADED",
			"execution-policy consent gate unavailable — order rejected (fail closed)")
	}
	return g.Policy.CheckConsent(ctx, accountID, reduceOnly)
}

// Consent records the client's acknowledgement of a policy version —
// writes execution_policy_consents + account_consents EXECUTION_POLICY
// in the same transaction; idempotent on (account, version) replays.
// Only the ACTIVE version is consentable — a stale-version
// acknowledgement never satisfies the gate.
func (s *ExecutionPolicyService) Consent(ctx context.Context, accountID,
	userID int64, version, ip string, metadata json.RawMessage) (*PolicyConsent, bool, error) {
	p, err := s.active(ctx)
	if err != nil {
		return nil, false, err
	}
	if p == nil {
		return nil, false, excerrors.New("NOT_FOUND",
			"no ACTIVE execution policy to consent to")
	}
	if version != "" && version != p.Version {
		return nil, false, excerrors.New("INVALID_REQUEST",
			"consent must target the ACTIVE version "+p.Version+
				" — version "+version+" is stale")
	}
	if len(metadata) == 0 {
		metadata = json.RawMessage("{}")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "consent tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO execution_policy_consents
		    (account_id, policy_id, version, consented_by, ip)
		VALUES ($1,$2,$3,$4,NULLIF($5,''))
		ON CONFLICT (account_id, policy_id) DO NOTHING
		RETURNING id`,
		accountID, p.ID, p.Version, userID, ip).Scan(&id)
	if err != nil && err != pgx.ErrNoRows {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "consent insert", err)
	}
	created := err != pgx.ErrNoRows
	if _, err := tx.Exec(ctx, `
		INSERT INTO account_consents
		    (account_id, consent_type, doc_ref, consented_by, ip, metadata)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),$6)
		ON CONFLICT (account_id, consent_type, doc_ref) DO NOTHING`,
		accountID, ConsentExecPolicy, p.Version, userID, ip, metadata); err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "gate consent insert", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "consent commit", err)
	}
	if !created {
		existing, gerr := s.consentByVersion(ctx, accountID, p.Version)
		if gerr != nil {
			return nil, false, gerr
		}
		return existing, false, nil
	}
	return &PolicyConsent{ID: id, AccountID: accountID, PolicyID: p.ID,
		Version: p.Version, ConsentedAt: s.now().UTC(),
		ConsentedBy: &userID, IP: ip}, true, nil
}

// ConsentStatus reports the account's standing relative to the ACTIVE
// version — the consent surface's read.
func (s *ExecutionPolicyService) ConsentStatus(ctx context.Context,
	accountID int64) (*PolicyConsent, bool, error) {
	p, err := s.active(ctx)
	if err != nil {
		return nil, false, err
	}
	if p == nil {
		return nil, false, nil
	}
	c, err := s.consentByVersion(ctx, accountID, p.Version)
	if err != nil {
		return nil, false, err
	}
	return c, c != nil, nil
}

func (s *ExecutionPolicyService) consentByVersion(ctx context.Context,
	accountID int64, version string) (*PolicyConsent, error) {
	var c PolicyConsent
	var ip *string
	err := s.pool.QueryRow(ctx, `
		SELECT id, account_id, policy_id, version, consented_at,
		       consented_by, ip
		FROM execution_policy_consents
		WHERE account_id=$1 AND version=$2`, accountID, version).
		Scan(&c.ID, &c.AccountID, &c.PolicyID, &c.Version, &c.ConsentedAt,
			&c.ConsentedBy, &ip)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "consent load", err)
	}
	if ip != nil {
		c.IP = *ip
	}
	return &c, nil
}

// ---------------------------------------------------------------------------
// Reads + sweep
// ---------------------------------------------------------------------------

// Active returns the publicly served ACTIVE policy (nil when none).
func (s *ExecutionPolicyService) Active(ctx context.Context) (*ExecutionPolicy, error) {
	return s.active(ctx)
}

func (s *ExecutionPolicyService) active(ctx context.Context) (*ExecutionPolicy, error) {
	p, err := scanPolicy(s.pool.QueryRow(ctx,
		`SELECT `+policyCols+` FROM execution_policies WHERE status='ACTIVE'`))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "policy active", err)
	}
	s.annotate(p)
	return p, nil
}

// List returns the version history newest-first (admin surface).
func (s *ExecutionPolicyService) List(ctx context.Context, limit int) ([]ExecutionPolicy, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+policyCols+` FROM execution_policies
		 ORDER BY id DESC LIMIT `+fmt.Sprint(limit))
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "policy list", err)
	}
	defer rows.Close()
	var out []ExecutionPolicy
	for rows.Next() {
		p, err := scanPolicyRows(rows)
		if err != nil {
			return nil, err
		}
		s.annotate(p)
		out = append(out, *p)
	}
	return out, rows.Err()
}

// SweepOverdueReview pages P1 while the ACTIVE policy's annual review
// is overdue — the alert is idempotent per policy (audit-chain marker,
// same dedup ledger as the regulatory-change sweep).
func (s *ExecutionPolicyService) SweepOverdueReview(ctx context.Context) (int, error) {
	now := s.now().UTC()
	var id int64
	var due *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT id, review_due_at FROM execution_policies
		WHERE status='ACTIVE' AND review_due_at < $1`, now).Scan(&id, &due)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "policy overdue probe", err)
	}
	var marked bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM audit_hash_chain
			WHERE table_name='execution_policies' AND record_id=$1
			  AND action='EXEC_POLICY_REVIEW_PAGED')`, id).Scan(&marked); err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "policy page dedup", err)
	}
	if marked || s.alerter == nil {
		return 0, nil
	}
	if err := s.alerter.RaiseHold(ctx, HoldAlert{
		Severity: "P1", Code: PolicyReviewOverdueCode,
		Summary: fmt.Sprintf("execution policy %d annual review overdue (due %s) — "+
			"version upgrades frozen", id, due.Format("2006-01-02")),
		Details: map[string]string{"policy_id": fmt.Sprint(id)},
	}); err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "policy page", err)
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "execution_policies",
		&id, "POLICY_REV_PAGED", nil); err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "policy page marker", err)
	}
	return 1, nil
}

func (s *ExecutionPolicyService) get(ctx context.Context,
	id int64) (*ExecutionPolicy, error) {
	p, err := scanPolicy(s.pool.QueryRow(ctx,
		`SELECT `+policyCols+` FROM execution_policies WHERE id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "execution policy not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "policy get", err)
	}
	s.annotate(p)
	return p, nil
}

// annotate stamps the overdue-review alert code on ACTIVE rows.
func (s *ExecutionPolicyService) annotate(p *ExecutionPolicy) {
	if p.Status == PolicyActive && p.ReviewDueAt != nil &&
		p.ReviewDueAt.Before(s.now().UTC()) {
		p.Code = PolicyReviewOverdueCode
	}
}

const policyCols = `
	id, version, body_ref, status, effective_from, review_due_at,
	material_change, approver, approved_at, evidence_pack, review_notes,
	reviewed_by, reviewed_at, created_by, created_at, updated_at`

func scanPolicy(row rowScanner) (*ExecutionPolicy, error) {
	var p ExecutionPolicy
	var notes *string
	err := row.Scan(&p.ID, &p.Version, &p.BodyRef, &p.Status,
		&p.EffectiveFrom, &p.ReviewDueAt, &p.MaterialChange, &p.Approver,
		&p.ApprovedAt, &p.EvidencePack, &notes, &p.ReviewedBy,
		&p.ReviewedAt, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if notes != nil {
		p.ReviewNotes = *notes
	}
	return &p, nil
}

func scanPolicyRows(rows pgx.Rows) (*ExecutionPolicy, error) {
	var p ExecutionPolicy
	var notes *string
	err := rows.Scan(&p.ID, &p.Version, &p.BodyRef, &p.Status,
		&p.EffectiveFrom, &p.ReviewDueAt, &p.MaterialChange, &p.Approver,
		&p.ApprovedAt, &p.EvidencePack, &notes, &p.ReviewedBy,
		&p.ReviewedAt, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "policy scan", err)
	}
	if notes != nil {
		p.ReviewNotes = *notes
	}
	return &p, nil
}

func (s *ExecutionPolicyService) checkRole(ctx context.Context, userID int64) error {
	role, err := s.resolver(ctx, userID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if role != "Compliance Officer" && role != "Super Admin" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot administer the execution policy")
	}
	return nil
}
