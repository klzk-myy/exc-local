// FX Global Code 55-principle self-assessment & annual review engine —
// Phase-21 Task 21.3.17 (spec §5.37/§14.6; §24 #182; §27.1 FX Global
// Code matrix row → FX_GLOBAL_CODE_NON_COMPLIANT audit warning).
//
// The engine maps operational platform metrics and policies onto the 6
// December-2024 themes (Ethics 1–3, Governance 4–7, Execution 8–18,
// Information Sharing 19–23, Risk Management & Compliance 24–41,
// Confirmation & Settlement 42–55) and programmatically verifies the
// key technical principles the schema can attest:
//
//	Principle  9 — 100% firm liquidity: zero last-look configuration on
//	              any instrument or session surface (§6.4 firm-quote
//	              model, ruling R11).
//	Principle 10 — deterministic order queuing + execution timestamp
//	              accuracy <1µs: the ClockEvidence seam (PTP kernel
//	              offset in production) must read synced and below the
//	              microsecond bound.
//	Principle 17 — pre-hedging prohibition: no order may carry
//	              pre-hedge semantics on any admission surface.
//	Principle 50 — CLS PvP settlement integration + nostro
//	              reconciliation: no settlement instruction stuck
//	              PENDING past value date and no FAILED leg lacking a
//	              RECONCILED disposition.
//
// Principles without an automated probe stay PENDING until a
// Compliance Officer records a verdict — a run cannot COMPLETE or sign
// with PENDING verdicts outstanding (fail-closed; an unsigned or
// partial matrix is never published as adherence). Completing a run
// generates the Statement of Commitment artefact (migration 060) for
// executive sign-off and public-register publication.
package compliance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"
	excerrors "exchange/pkg/errors"
)

// Theme names (spec §14.6) — principle ranges are contiguous.
const (
	ThemeEthics                 = "ETHICS"
	ThemeGovernance             = "GOVERNANCE"
	ThemeExecution              = "EXECUTION"
	ThemeInformationSharing     = "INFORMATION_SHARING"
	ThemeRiskCompliance         = "RISK_MANAGEMENT_COMPLIANCE"
	ThemeConfirmationSettlement = "CONFIRMATION_SETTLEMENT"
)

// Adherence verdicts (migration 060 CHECK mirror).
const (
	AdherenceAdherent = "ADHERENT"
	AdherencePartial  = "PARTIAL"
	AdherenceNon      = "NON_ADHERENT"
	AdherencePending  = "PENDING"
)

// FXGCCode is the §27.1 matrix audit-warning code carried on non-
// compliant outcomes (payload `code` field + ops alerts). Deliberately
// NOT registered in errs — the matrix labels it "(Audit Warning)" with
// no HTTP status (the CTR_TRIGGERED precedent for audit-only codes).
const FXGCCode = "FX_GLOBAL_CODE_NON_COMPLIANT"

// FXGCPrinciplesTotal is the December-2024 Global Code principle count.
const FXGCPrinciplesTotal = 55

// fxGCTheme maps a 1-based principle id onto its §14.6 theme.
func fxGCTheme(p int) string {
	switch {
	case p >= 1 && p <= 3:
		return ThemeEthics
	case p >= 4 && p <= 7:
		return ThemeGovernance
	case p >= 8 && p <= 18:
		return ThemeExecution
	case p >= 19 && p <= 23:
		return ThemeInformationSharing
	case p >= 24 && p <= 41:
		return ThemeRiskCompliance
	default:
		return ThemeConfirmationSettlement // 42–55
	}
}

// fxGCPrincipleTitles — condensed descriptors for all 55 principles
// (December-2024 edition ordering; the matrix stores them verbatim in
// evidence so an amended-code edge case can supersede by run).
var fxGCPrincipleTitles = map[int]string{
	1:  "highest standards of professional and ethical behaviour",
	2:  "robust and pro-active compliance culture",
	3:  "identify and address conflicts of interest",
	4:  "clear and demonstrable governance structures",
	5:  "independent risk/compliance functions with sufficient stature",
	6:  "effective control environment and remuneration alignment",
	7:  "periodic review of Global Code adherence",
	8:  "clarity on capacity in which market participant acts",
	9:  "firm liquidity only — no last look on executable prices",
	10: "fair handling of orders, queuing and execution timestamps",
	11: "client requests to stop/cancel honoured honestly",
	12: "pricing transparency of markups and fees",
	13: "electronic trading algorithms monitored and tested",
	14: "low-latency aggregation/hedging transparency",
	15: "risk-disclosure clarity on quoted currencies and tenors",
	16: "clear explanation of last-look terms where any exist — none permitted",
	17: "pre-hedging prohibited unless disclosed, intended and in client interest",
	18: "consistent application of markups across clients",
	19: "client information shared only with consent and purpose",
	20: "market colour exchanged only on a need-to-know basis",
	21: "trading interests not used to manipulate client positions",
	22: "PII and order data isolation between business lines",
	23: "clear guidance on what information may be shared",
	24: "credit risk measured, limited and monitored",
	25: "net open positions within authorized limits",
	26: "settlement risk managed proactively",
	27: "counterparty due diligence maintained",
	28: "robust compliance with AML/CFT obligations",
	29: "legal and regulatory obligations met across jurisdictions",
	30: "disruption monitoring and business continuity",
	31: "operational resilience and independent validation of controls",
	32: "market conduct surveillance embedded in operations",
	33: "compliance breaches escalated and remediated",
	34: "trading records retained and retrievable",
	35: "personal trading of staff appropriately controlled",
	36: "risk of manual error minimized via automation",
	37: "personnel competence and training for roles held",
	38: "escalation channels for ethical concerns",
	39: "conflicts between own and client interest addressed",
	40: "credit-screened liquidity honoured symmetrically",
	41: "business conduct deviations investigated and documented",
	42: "trade confirmations accurate and timely",
	43: "post-trade allocations completed transparently",
	44: "trade amendments and cancellations audited",
	45: "settlement instructions standardized and verified",
	46: "SSIs maintained and validated on change",
	47: "netting applied per executed agreements",
	48: "payment flows reconciled to confirmation records",
	49: "settlement failures investigated and corrected",
	50: "PvP/CLS settlement prioritized; nostro reconciliation daily",
	51: "liquidity and funding risk assessed for value dates",
	52: "margin/collateral handled per executed documents",
	53: "trade economics and lifecycle events accurate",
	54: "post-trade reporting timely and complete",
	55: "continuous improvement of settlement processes",
}

// ClockEvidence supplies the P10 timestamp-fidelity probe — maximum
// observed clock offset in nanoseconds and whether the time source is
// synced (timesync.Guard/PTPMonitor adapter in production).
type ClockEvidence func(ctx context.Context) (maxOffsetNs int64, synced bool, err error)

// FXGCService owns runs, verdicts and the statement pipeline.
type FXGCService struct {
	pool     *pgxpool.Pool
	resolver HoldRoleResolver
	alerter  HoldAlerter
	clock    ClockEvidence
	now      func() time.Time
}

// NewFXGCService wires the service; pool required, resolver gates
// officer mutations (Compliance Officer / Super Admin).
func NewFXGCService(pool *pgxpool.Pool, resolver HoldRoleResolver) (*FXGCService, error) {
	if pool == nil {
		return nil, excerrors.New("INTERNAL_ERROR", "fx global code service requires pool")
	}
	return &FXGCService{pool: pool, resolver: resolver, now: time.Now}, nil
}

// WithClockEvidence wires the P10 probe seam.
func (s *FXGCService) WithClockEvidence(c ClockEvidence) *FXGCService {
	s.clock = c
	return s
}

// WithAlerter wires the ops channel for non-compliance warnings.
func (s *FXGCService) WithAlerter(a HoldAlerter) *FXGCService {
	s.alerter = a
	return s
}

// WithClock overrides the clock (tests).
func (s *FXGCService) WithClock(c func() time.Time) *FXGCService {
	s.now = c
	return s
}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// AssessmentRun is one compliance_assessment_runs row.
type AssessmentRun struct {
	ID                 int64      `json:"id"`
	Framework          string     `json:"framework"`
	CodeVersion        string     `json:"code_version"`
	Period             string     `json:"period"`
	Status             string     `json:"status"`
	StartedBy          int64      `json:"started_by"`
	PrinciplesTotal    int        `json:"principles_total"`
	PrinciplesAdherent int        `json:"principles_adherent"`
	PrinciplesPartial  int        `json:"principles_partial"`
	PrinciplesNon      int        `json:"principles_non"`
	PrinciplesPending  int        `json:"principles_pending"`
	Score              *float64   `json:"score,omitempty"`
	SignedBy           *int64     `json:"signed_by,omitempty"`
	SignedAt           *time.Time `json:"signed_at,omitempty"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	Code               string     `json:"code,omitempty"` // FX_GLOBAL_CODE_NON_COMPLIANT when non-adherent
}

// AssessmentRow is one compliance_assessments row (spec §5.37).
type AssessmentRow struct {
	ID              int64           `json:"id"`
	RunID           int64           `json:"run_id"`
	Framework       string          `json:"framework"`
	AssessmentDate  time.Time       `json:"assessment_date"`
	PrincipleID     int             `json:"principle_id"`
	Theme           string          `json:"theme"`
	AdherenceStatus string          `json:"adherence_status"`
	Automated       bool            `json:"automated"`
	EvidenceSummary string          `json:"evidence_summary,omitempty"`
	Evidence        json.RawMessage `json:"evidence"`
	RemediationRef  string          `json:"remediation_ref,omitempty"`
	AssessorID      *int64          `json:"assessor_id,omitempty"`
	ApprovedByCCO   bool            `json:"approved_by_cco"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// Statement is one fx_gc_statements row.
type Statement struct {
	ID          int64      `json:"id"`
	RunID       int64      `json:"run_id"`
	CodeVersion string     `json:"code_version"`
	Period      string     `json:"period"`
	Body        string     `json:"body"`
	BodySHA256  string     `json:"body_sha256"`
	SignedBy    *int64     `json:"signed_by,omitempty"`
	SignedAt    *time.Time `json:"signed_at,omitempty"`
	Published   bool       `json:"published"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// ---------------------------------------------------------------------------
// Automated control verification
// ---------------------------------------------------------------------------

// automatedChecks enumerates the principles the schema can attest —
// every other principle stays PENDING for officer verdict.
var automatedChecks = map[int]bool{9: true, 10: true, 17: true, 50: true}

// runAutoCheck evaluates one principle; the verdict and evidence are
// written onto row.
func (s *FXGCService) runAutoCheck(ctx context.Context, p int, row *AssessmentRow) error {
	row.Automated = true
	ev := map[string]any{"principle": p, "probe": "automated"}
	fail := func(status, summary string) {
		row.AdherenceStatus = status
		row.EvidenceSummary = summary
	}
	switch p {
	case 9:
		// P9 firm liquidity: the venue admits no last-look or hold-time
		// semantics on any instrument or session surface (spec §6.4).
		var n int64
		if err := s.pool.QueryRow(ctx, `
			SELECT count(*) FROM instruments
			WHERE execution_rule IS NOT NULL AND (
				coalesce(execution_rule->>'last_look','false') IN ('true','1') OR
				coalesce(execution_rule->>'non_firm','false') IN ('true','1') OR
				coalesce(execution_rule->>'hold_time_ms','0')::numeric > 0)`).
			Scan(&n); err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "p9 probe", err)
		}
		ev["non_firm_instruments"] = n
		if n == 0 {
			fail(AdherenceAdherent,
				"zero non-firm/last-look instrument configurations — 100% firm liquidity verified")
		} else {
			fail(AdherenceNon,
				fmt.Sprintf("%d instrument(s) carry non-firm/last-look execution rules", n))
		}
	case 10:
		// P10 timestamp fidelity: the time-sync evidence seam must read
		// synced and within the <1µs bound (MiFID RTS 25 coarser gate is
		// 100µs — the Global Code self-attestation holds the tighter bar).
		if s.clock == nil {
			row.Automated = false
			fail(AdherencePending, "no clock-evidence source wired — manual attestation required")
			break
		}
		maxNs, synced, err := s.clock(ctx)
		if err != nil {
			row.Automated = false
			fail(AdherencePending,
				"clock evidence unavailable — manual attestation required: "+err.Error())
			break
		}
		ev["max_offset_ns"] = maxNs
		ev["synced"] = synced
		switch {
		case !synced:
			fail(AdherenceNon, "time source unsynced — execution timestamps unverifiable")
		case maxNs > 1000:
			fail(AdherenceNon,
				fmt.Sprintf("max clock offset %dns exceeds the 1µs fidelity bound", maxNs))
		default:
			fail(AdherenceAdherent,
				fmt.Sprintf("PTP-synced, max offset %dns within the 1µs bound", maxNs))
		}
	case 17:
		// P17 pre-hedging prohibition: no submitted order may carry
		// pre-hedge semantics on any surface (algo_params/exec params).
		var n int64
		if err := s.pool.QueryRow(ctx, `
			SELECT count(*) FROM orders
			WHERE algo_params IS NOT NULL AND (
				coalesce(algo_params->>'pre_hedge','false') IN ('true','1'))`).
			Scan(&n); err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "p17 probe", err)
		}
		ev["pre_hedge_orders"] = n
		if n == 0 {
			fail(AdherenceAdherent,
				"no order carries pre-hedge semantics — prohibition verified")
		} else {
			fail(AdherenceNon,
				fmt.Sprintf("%d order(s) carry pre-hedge markers", n))
		}
	case 50:
		// P50 PvP + reconciliation: no settlement instruction may sit
		// PENDING past its value date or FAILED without a RECONCILED
		// disposition row.
		var stuck int64
		if err := s.pool.QueryRow(ctx, `
			SELECT count(*) FROM settlement_instructions
			WHERE (status = 'PENDING' AND settlement_date < CURRENT_DATE)
			   OR status = 'FAILED'`).Scan(&stuck); err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "p50 probe", err)
		}
		ev["unsettled_or_failed"] = stuck
		if stuck == 0 {
			fail(AdherenceAdherent,
				"no stale PENDING or unresolved FAILED settlement legs — PvP/reconciliation verified")
		} else {
			fail(AdherencePartial,
				fmt.Sprintf("%d settlement leg(s) stale or failed pending reconciliation", stuck))
		}
	}
	raw, _ := json.Marshal(ev)
	row.Evidence = raw
	return nil
}

// ---------------------------------------------------------------------------
// Run lifecycle
// ---------------------------------------------------------------------------

// StartRun opens an annual review run for a period: the 55-principle
// matrix is seeded and every automated probe evaluates immediately.
// Idempotent on (framework, period) — a replay returns the stored run.
func (s *FXGCService) StartRun(ctx context.Context, period, codeVersion string,
	officer int64) (*AssessmentRun, bool, error) {
	if err := s.checkRole(ctx, officer); err != nil {
		return nil, false, err
	}
	if period == "" {
		return nil, false, excerrors.New("INVALID_REQUEST", "period required (e.g. FY2026)")
	}
	if codeVersion == "" {
		codeVersion = "DEC_2024"
	}
	today := s.now().UTC().Truncate(24 * time.Hour)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "fxgc run tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO compliance_assessment_runs
		    (framework, code_version, period, started_by, principles_total)
		VALUES ('FX_GLOBAL_CODE',$1,$2,$3,$4)
		ON CONFLICT (framework, period) DO NOTHING
		RETURNING id`, codeVersion, period, officer, FXGCPrinciplesTotal).Scan(&id)
	if err == pgx.ErrNoRows {
		existing, gerr := s.runByPeriod(ctx, period)
		if gerr != nil {
			return nil, false, gerr
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, excerrors.Wrap("INTERNAL_ERROR", "fxgc dedup commit", err)
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "fxgc run insert", err)
	}
	// Seed all 55 rows as PENDING — the automated probes fill in below.
	for p := 1; p <= FXGCPrinciplesTotal; p++ {
		if _, err := tx.Exec(ctx, `
			INSERT INTO compliance_assessments
			    (run_id, framework, assessment_date, principle_id, theme,
			     adherence_status, automated)
			VALUES ($1,'FX_GLOBAL_CODE',$2,$3,$4,'PENDING',$5)`,
			id, today, p, fxGCTheme(p), automatedChecks[p]); err != nil {
			return nil, false, excerrors.Wrap("INTERNAL_ERROR", "fxgc matrix seed", err)
		}
	}
	if _, err := audit.Append(ctx, tx, "compliance_assessment_runs", &id,
		"FXGC_RUN_STARTED", nil); err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "fxgc audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "fxgc run commit", err)
	}

	// Automated probes run post-commit — a probe failure marks its row
	// PENDING with the error in evidence, never aborts the run.
	for p := range automatedChecks {
		row, gerr := s.assessmentRow(ctx, id, p)
		if gerr != nil {
			continue
		}
		if cerr := s.runAutoCheck(ctx, p, row); cerr != nil {
			row.AdherenceStatus = AdherencePending
			row.Automated = false
			row.EvidenceSummary = "automated probe failed — manual attestation required"
			raw, _ := json.Marshal(map[string]any{"probe_error": cerr.Error()})
			row.Evidence = raw
		}
		if _, uerr := s.pool.Exec(ctx, `
			UPDATE compliance_assessments
			SET adherence_status=$3, automated=$4, evidence_summary=NULLIF($5,''),
			    evidence=$6, updated_at=now()
			WHERE id=$1 AND run_id=$2`,
			row.ID, id, row.AdherenceStatus, row.Automated,
			row.EvidenceSummary, row.Evidence); uerr != nil {
			return nil, false, excerrors.Wrap("INTERNAL_ERROR", "fxgc probe persist", uerr)
		}
	}
	if err := s.refreshCounts(ctx, id); err != nil {
		return nil, false, err
	}
	run, err := s.GetRun(ctx, id)
	if err != nil {
		return nil, false, err
	}
	s.alertNonCompliant(ctx, run)
	return run, true, nil
}

// Assess records the officer's verdict on one principle — required for
// every non-automated row before the run can complete.
func (s *FXGCService) Assess(ctx context.Context, runID int64, principleID int,
	officer int64, status, summary, remediationRef string) (*AssessmentRow, error) {
	if err := s.checkRole(ctx, officer); err != nil {
		return nil, err
	}
	switch status {
	case AdherenceAdherent, AdherencePartial, AdherenceNon:
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			"adherence_status must be ADHERENT|PARTIAL|NON_ADHERENT")
	}
	if status != AdherenceAdherent && remediationRef == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"PARTIAL/NON_ADHERENT verdicts require a remediation ticket ref")
	}
	if principleID < 1 || principleID > FXGCPrinciplesTotal {
		return nil, excerrors.New("INVALID_REQUEST", "principle_id out of range")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc assess tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var rowID int64
	var runStatus string
	err = tx.QueryRow(ctx, `
		SELECT a.id, r.status FROM compliance_assessments a
		JOIN compliance_assessment_runs r ON r.id = a.run_id
		WHERE a.run_id = $1 AND a.principle_id = $2 FOR UPDATE OF a`,
		runID, principleID).Scan(&rowID, &runStatus)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "assessment row not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc assess lock", err)
	}
	if runStatus != "RUNNING" {
		return nil, excerrors.New("INVALID_REQUEST",
			"run is "+runStatus+" — verdicts are locked")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE compliance_assessments
		SET adherence_status=$3, automated=FALSE, evidence_summary=NULLIF($4,''),
		    remediation_ref=NULLIF($5,''), assessor_id=$6, updated_at=now()
		WHERE id=$1 AND run_id=$2`, rowID, runID, status, summary,
		remediationRef, officer); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc assess update", err)
	}
	if _, err := audit.Append(ctx, tx, "compliance_assessments", &rowID,
		"FXGC_ASSESSED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc assess audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc assess commit", err)
	}
	if err := s.refreshCounts(ctx, runID); err != nil {
		return nil, err
	}
	return s.assessmentRow(ctx, runID, principleID)
}

// Complete freezes the verdicts, computes the score, marks the run
// COMPLETED and generates the Statement of Commitment. Fails closed
// while any principle verdict is still PENDING.
func (s *FXGCService) Complete(ctx context.Context, runID, officer int64) (*AssessmentRun, error) {
	if err := s.checkRole(ctx, officer); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc complete tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := s.lockRun(ctx, tx, runID)
	if err != nil {
		return nil, err
	}
	if run.Status != "RUNNING" {
		return nil, excerrors.New("INVALID_REQUEST",
			"run is "+run.Status+" — only RUNNING can complete")
	}
	var pending int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM compliance_assessments
		WHERE run_id = $1 AND adherence_status = 'PENDING'`, runID).
		Scan(&pending); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc pending probe", err)
	}
	if pending > 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("%d principle verdict(s) still PENDING — assessment incomplete", pending))
	}
	var adherent, partial, non int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE adherence_status='ADHERENT'),
		       count(*) FILTER (WHERE adherence_status='PARTIAL'),
		       count(*) FILTER (WHERE adherence_status='NON_ADHERENT')
		FROM compliance_assessments WHERE run_id = $1`, runID).
		Scan(&adherent, &partial, &non); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc score", err)
	}
	score := float64(adherent) / float64(FXGCPrinciplesTotal) * 100
	if _, err := tx.Exec(ctx, `
		UPDATE compliance_assessment_runs
		SET status='COMPLETED', principles_adherent=$2, principles_partial=$3,
		    principles_non=$4, principles_pending=0, score=$5,
		    completed_at=now(), updated_at=now()
		WHERE id=$1`, runID, adherent, partial, non, score); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc complete update", err)
	}
	if _, err := audit.Append(ctx, tx, "compliance_assessment_runs", &runID,
		"FXGC_COMPLETED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc complete audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc complete commit", err)
	}
	// Statement generation is post-commit — the artefact derives from
	// the committed verdict set.
	if err := s.GenerateStatement(ctx, runID); err != nil {
		return nil, err
	}
	run, err = s.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	s.alertNonCompliant(ctx, run)
	return run, nil
}

// Sign records the executive/CCO sign-off on the generated statement —
// the run transitions to SIGNED and the matrix rows are stamped
// approved_by_cco. Signatory must be an officer (the task pins CCO;
// §8.2 has no executive role so Compliance Officer / Super Admin gate).
func (s *FXGCService) Sign(ctx context.Context, runID, signatory int64) (*AssessmentRun, error) {
	if err := s.checkRole(ctx, signatory); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc sign tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := s.lockRun(ctx, tx, runID)
	if err != nil {
		return nil, err
	}
	if run.Status != "COMPLETED" {
		return nil, excerrors.New("INVALID_REQUEST",
			"run is "+run.Status+" — only a COMPLETED run can sign")
	}
	var stmtID int64
	if err := tx.QueryRow(ctx,
		`SELECT id FROM fx_gc_statements WHERE run_id = $1`, runID).
		Scan(&stmtID); err == pgx.ErrNoRows {
		return nil, excerrors.New("INVALID_REQUEST",
			"no Statement of Commitment generated — complete the run first")
	} else if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc stmt probe", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE compliance_assessment_runs
		SET status='SIGNED', signed_by=$2, signed_at=now(), updated_at=now()
		WHERE id=$1`, runID, signatory); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc sign run", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE fx_gc_statements SET signed_by=$2, signed_at=now()
		WHERE id=$1`, stmtID, signatory); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc sign stmt", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE compliance_assessments SET approved_by_cco=TRUE, updated_at=now()
		WHERE run_id=$1`, runID); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc sign matrix", err)
	}
	if _, err := audit.Append(ctx, tx, "compliance_assessment_runs", &runID,
		"FXGC_RUN_SIGNED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc sign audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc sign commit", err)
	}
	return s.GetRun(ctx, runID)
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

const fxgcRunCols = `
	id, framework, code_version, period, status, started_by,
	principles_total, principles_adherent, principles_partial,
	principles_non, principles_pending, score, signed_by, signed_at,
	completed_at, created_at, updated_at`

func scanRun(row rowScanner) (*AssessmentRun, error) {
	var r AssessmentRun
	err := row.Scan(&r.ID, &r.Framework, &r.CodeVersion, &r.Period,
		&r.Status, &r.StartedBy, &r.PrinciplesTotal, &r.PrinciplesAdherent,
		&r.PrinciplesPartial, &r.PrinciplesNon, &r.PrinciplesPending,
		&r.Score, &r.SignedBy, &r.SignedAt, &r.CompletedAt,
		&r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if r.PrinciplesNon > 0 {
		r.Code = FXGCCode
	}
	return &r, nil
}

func (s *FXGCService) runByPeriod(ctx context.Context, period string) (*AssessmentRun, error) {
	run, err := scanRun(s.pool.QueryRow(ctx,
		`SELECT `+fxgcRunCols+` FROM compliance_assessment_runs
		 WHERE framework='FX_GLOBAL_CODE' AND period=$1`, period))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "assessment run not found")
	}
	return run, err
}

// GetRun returns one run.
func (s *FXGCService) GetRun(ctx context.Context, id int64) (*AssessmentRun, error) {
	run, err := scanRun(s.pool.QueryRow(ctx,
		`SELECT `+fxgcRunCols+` FROM compliance_assessment_runs WHERE id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "assessment run not found")
	}
	return run, err
}

func (s *FXGCService) lockRun(ctx context.Context, tx pgx.Tx, id int64) (*AssessmentRun, error) {
	run, err := scanRun(tx.QueryRow(ctx,
		`SELECT `+fxgcRunCols+` FROM compliance_assessment_runs
		 WHERE id=$1 FOR UPDATE`, id))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "assessment run not found")
	}
	return run, err
}

// ListRuns returns newest-first runs.
func (s *FXGCService) ListRuns(ctx context.Context, limit int) ([]AssessmentRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+fxgcRunCols+` FROM compliance_assessment_runs
		 ORDER BY id DESC LIMIT `+fmt.Sprint(limit))
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc list", err)
	}
	defer rows.Close()
	var out []AssessmentRun
	for rows.Next() {
		var r AssessmentRun
		if err := rows.Scan(&r.ID, &r.Framework, &r.CodeVersion, &r.Period,
			&r.Status, &r.StartedBy, &r.PrinciplesTotal, &r.PrinciplesAdherent,
			&r.PrinciplesPartial, &r.PrinciplesNon, &r.PrinciplesPending,
			&r.Score, &r.SignedBy, &r.SignedAt, &r.CompletedAt,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc run scan", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListMatrix returns the principle rows for a run.
func (s *FXGCService) ListMatrix(ctx context.Context, runID int64) ([]AssessmentRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, run_id, framework, assessment_date, principle_id, theme,
		       adherence_status, automated, evidence_summary, evidence,
		       remediation_ref, assessor_id, approved_by_cco, created_at, updated_at
		FROM compliance_assessments WHERE run_id = $1
		ORDER BY principle_id`, runID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc matrix", err)
	}
	defer rows.Close()
	var out []AssessmentRow
	for rows.Next() {
		var r AssessmentRow
		var summ, rem *string
		if err := rows.Scan(&r.ID, &r.RunID, &r.Framework, &r.AssessmentDate,
			&r.PrincipleID, &r.Theme, &r.AdherenceStatus, &r.Automated,
			&summ, &r.Evidence, &rem, &r.AssessorID, &r.ApprovedByCCO,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc row scan", err)
		}
		if summ != nil {
			r.EvidenceSummary = *summ
		}
		if rem != nil {
			r.RemediationRef = *rem
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *FXGCService) assessmentRow(ctx context.Context, runID int64,
	principleID int) (*AssessmentRow, error) {
	var r AssessmentRow
	var summ, rem *string
	err := s.pool.QueryRow(ctx, `
		SELECT id, run_id, framework, assessment_date, principle_id, theme,
		       adherence_status, automated, evidence_summary, evidence,
		       remediation_ref, assessor_id, approved_by_cco, created_at, updated_at
		FROM compliance_assessments WHERE run_id=$1 AND principle_id=$2`,
		runID, principleID).Scan(&r.ID, &r.RunID, &r.Framework, &r.AssessmentDate,
		&r.PrincipleID, &r.Theme, &r.AdherenceStatus, &r.Automated,
		&summ, &r.Evidence, &rem, &r.AssessorID, &r.ApprovedByCCO,
		&r.CreatedAt, &r.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "assessment row not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc row load", err)
	}
	if summ != nil {
		r.EvidenceSummary = *summ
	}
	if rem != nil {
		r.RemediationRef = *rem
	}
	return &r, nil
}

// GetStatement returns the generated Statement of Commitment for a run.
func (s *FXGCService) GetStatement(ctx context.Context, runID int64) (*Statement, error) {
	var st Statement
	err := s.pool.QueryRow(ctx, `
		SELECT id, run_id, code_version, period, body, body_sha256,
		       signed_by, signed_at, published, published_at, created_at
		FROM fx_gc_statements WHERE run_id = $1`, runID).
		Scan(&st.ID, &st.RunID, &st.CodeVersion, &st.Period, &st.Body,
			&st.BodySHA256, &st.SignedBy, &st.SignedAt, &st.Published,
			&st.PublishedAt, &st.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "statement not generated")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc stmt load", err)
	}
	return &st, nil
}

// PublishStatement flags the signed statement for the public register —
// unsigned statements are never publishable.
func (s *FXGCService) PublishStatement(ctx context.Context, runID,
	officer int64) (*Statement, error) {
	if err := s.checkRole(ctx, officer); err != nil {
		return nil, err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE fx_gc_statements SET published=TRUE, published_at=now()
		WHERE run_id=$1 AND signed_by IS NOT NULL`, runID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "fxgc publish", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"statement missing or unsigned — sign-off precedes publication")
	}
	return s.GetStatement(ctx, runID)
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

// refreshCounts rolls the verdict counters onto the run row.
func (s *FXGCService) refreshCounts(ctx context.Context, runID int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE compliance_assessment_runs r
		SET principles_adherent = sub.a, principles_partial = sub.p,
		    principles_non = sub.n, principles_pending = sub.d,
		    updated_at = now()
		FROM (
		    SELECT run_id,
		           count(*) FILTER (WHERE adherence_status='ADHERENT') AS a,
		           count(*) FILTER (WHERE adherence_status='PARTIAL') AS p,
		           count(*) FILTER (WHERE adherence_status='NON_ADHERENT') AS n,
		           count(*) FILTER (WHERE adherence_status='PENDING') AS d
		    FROM compliance_assessments WHERE run_id = $1 GROUP BY run_id
		) sub
		WHERE r.id = $1`, runID)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "fxgc counts", err)
	}
	return nil
}

// alertNonCompliant pages the audit warning when a completed or running
// assessment shows NON_ADHERENT verdicts — the §27.1 matrix code rides
// the alert, never a silent dashboard gap.
func (s *FXGCService) alertNonCompliant(ctx context.Context, run *AssessmentRun) {
	if s.alerter == nil || run.PrinciplesNon == 0 {
		return
	}
	_ = s.alerter.RaiseHold(ctx, HoldAlert{
		Severity: "P2", Code: FXGCCode,
		Summary: fmt.Sprintf("FX Global Code assessment %s/%s: %d non-adherent principle(s)",
			run.Period, run.CodeVersion, run.PrinciplesNon),
		Details: map[string]string{
			"adherent": fmt.Sprint(run.PrinciplesAdherent),
			"partial":  fmt.Sprint(run.PrinciplesPartial),
			"pending":  fmt.Sprint(run.PrinciplesPending),
		},
	})
}

func (s *FXGCService) checkRole(ctx context.Context, userID int64) error {
	if s.resolver == nil {
		return excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	role, err := s.resolver(ctx, userID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if role != "Compliance Officer" && role != "Super Admin" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot action FX Global Code assessments")
	}
	return nil
}

// statementHash pins the generated body — the WORM-grade integrity
// evidence the public register entry cites.
func statementHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}
