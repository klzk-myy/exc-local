// FinCEN MSB & AML program — Phase-21 Task 21.3.6 (spec §14.1/§14.3;
// §24 #105 CTR ≥ $10,000; §27.1 FinCEN MSB matrix row →
// MSB_COMPLIANCE_BREACH 500).
//
//   - CTR aggregation: every account whose DEPOSIT+WITHDRAWAL movement
//     in one UTC business day reaches $10,000 lands a ctr_reports row
//     (audit CTR_TRIGGERED) — deterministic, idempotent per
//     (account, business_date).
//   - Structuring/smurfing: >= 2 individually-sub-threshold legs whose
//     day total reaches the CTR threshold raise a STRUCTURING
//     monitoring event + an automatic SAR draft — the reportable
//     suspicion is the deliberate sub-threshold split, not the CTR
//     itself.
//   - Suspicion scoring: monitoring events carry points; a rolling
//     90-day account score >= 60 escalates an EDD assessment and drafts
//     a MONITORING_RULE SAR (weekly dedup anchor).
//   - Program register: aml_program_artifacts holds the FinCEN Form 107
//     MSB registration, risk assessments, versioned policies, training
//     logs, officer designation and the annual-review tracker;
//     ProgramStatus reports MSB_COMPLIANCE_BREACH detail for every
//     missing/expired mandatory artifact.
//
// All monetary math uses DECIMAL(28,8) — funding_transactions.usd_amount
// is the converted figure (USD rows fall back to amount); unpriced
// non-USD legs count toward the unpriced_count review flag.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/audit"
	excerrors "exchange/pkg/errors"
)

// CTRThresholdUSD is the FinCEN Currency Transaction Report floor —
// aggregate cash movement reaching $10,000 in a business day triggers
// the report (§24 #105).
var CTRThresholdUSD = decimal.RequireFromString("10000")

// Suspicion score weights + escalation floor.
const (
	ScoreStructuring = 50 // deliberate sub-threshold splitting
	ScoreCTRTrigger  = 20 // reportable volume, not itself suspicion
	ScoreVelocity    = 30 // rapid in-out cycling
	AMLScoreEDDFloor = 60 // rolling 90-day score → EDD + SAR draft
	amlScoreWindow   = 90 * 24 * time.Hour
)

// Monitoring rule ids (migration 033 CHECK mirror).
const (
	AMLRuleCTR          = "CTR_THRESHOLD"
	AMLRuleStructuring  = "STRUCTURING"
	AMLRuleVelocity     = "VELOCITY"
	AMLRuleDormant      = "DORMANT_REACTIVATION"
	AMLRuleRoundAmount  = "ROUND_AMOUNT"
	AMLRuleJurisdiction = "HIGH_RISK_JURISDICTION"
)

// Artifact kinds (migration 033 CHECK mirror).
const (
	ArtifactMSB          = "MSB_REGISTRATION"
	ArtifactRisk         = "RISK_ASSESSMENT"
	ArtifactPolicy       = "POLICY"
	ArtifactTraining     = "TRAINING"
	ArtifactOfficer      = "OFFICER_DESIGNATION"
	ArtifactAnnualReview = "ANNUAL_REVIEW"
)

// CTRReport is one ctr_reports row.
type CTRReport struct {
	ID            int64           `json:"id"`
	AccountID     int64           `json:"account_id"`
	BusinessDate  time.Time       `json:"business_date"`
	TxnCount      int             `json:"txn_count"`
	CashInUSD     decimal.Decimal `json:"cash_in_usd"`
	CashOutUSD    decimal.Decimal `json:"cash_out_usd"`
	TotalUSD      decimal.Decimal `json:"total_usd"`
	UnpricedCount int             `json:"unpriced_count"`
	Status        string          `json:"status"`
	SARID         *int64          `json:"sar_id,omitempty"`
	Detail        json.RawMessage `json:"detail"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// MonitoringEvent is one aml_monitoring_events row — a rule detection
// with its suspicion-score contribution.
type MonitoringEvent struct {
	ID           int64           `json:"id"`
	RuleID       string          `json:"rule_id"`
	AccountID    int64           `json:"account_id"`
	BusinessDate time.Time       `json:"business_date"`
	Score        int             `json:"score"`
	Detail       json.RawMessage `json:"detail"`
	DedupKey     string          `json:"dedup_key"`
	SARID        *int64          `json:"sar_id,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// AMLArtifact is one aml_program_artifacts row.
type AMLArtifact struct {
	ID           int64           `json:"id"`
	ArtifactType string          `json:"artifact_type"`
	Reference    string          `json:"reference,omitempty"`
	Title        string          `json:"title"`
	Version      string          `json:"version,omitempty"`
	Status       string          `json:"status"`
	Detail       json.RawMessage `json:"detail"`
	EffectiveAt  *time.Time      `json:"effective_at,omitempty"`
	ReviewDueAt  *time.Time      `json:"review_due_at,omitempty"`
	RecordedBy   *int64          `json:"recorded_by,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// AMLProgramStatus is the Task 21.3.6 program-health view — every
// breach carries the MSB_COMPLIANCE_BREACH detail for ops paging.
type AMLProgramStatus struct {
	Compliant bool          `json:"compliant"`
	Breaches  []string      `json:"breaches"`
	Code      string        `json:"code,omitempty"` // MSB_COMPLIANCE_BREACH when !Compliant
	Artifacts []AMLArtifact `json:"artifacts,omitempty"`
}

// AMLService owns CTR aggregation, monitoring-rule detection, suspicion
// scoring and the program register.
type AMLService struct {
	pool    *pgxpool.Pool
	sar     *SARService // nil → detections record but never draft
	alerter HoldAlerter // optional — reuses the hold ops-alert channel
	now     func() time.Time
}

// NewAMLService wires the service; the pool is required, sar may be nil
// (detections then stop short of drafting).
func NewAMLService(pool *pgxpool.Pool, sar *SARService) (*AMLService, error) {
	if pool == nil {
		return nil, excerrors.New("INTERNAL_ERROR", "aml service requires pool")
	}
	return &AMLService{pool: pool, sar: sar, now: time.Now}, nil
}

// WithAlerter wires the ops channel for program-breach pages.
func (s *AMLService) WithAlerter(a HoldAlerter) *AMLService {
	s.alerter = a
	return s
}

// WithClock overrides the clock (tests).
func (s *AMLService) WithClock(c func() time.Time) *AMLService {
	s.now = c
	return s
}

// ---------------------------------------------------------------------------
// CTR aggregation + structuring detection
// ---------------------------------------------------------------------------

// dayAggregate is the per-account business-day funding roll-up.
type dayAggregate struct {
	AccountID     int64
	TxnCount      int
	CashInUSD     decimal.Decimal
	CashOutUSD    decimal.Decimal
	TotalUSD      decimal.Decimal
	MaxSingleUSD  decimal.Decimal
	SubCount      int // individually-sub-threshold legs
	UnpricedCount int
	TxIDs         []int64
}

// ScanBusinessDay evaluates every account's movement inside [day 00:00,
// day+1 00:00) UTC. Idempotent — safe to re-run intra-day as more
// transactions land. Returns the number of CTR rows created/updated.
func (s *AMLService) ScanBusinessDay(ctx context.Context, day time.Time) (int, error) {
	start := time.Date(day.UTC().Year(), day.UTC().Month(), day.UTC().Day(),
		0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	rows, err := s.pool.Query(ctx, `
		SELECT account_id,
		       count(*),
		       coalesce(sum(usd) FILTER (WHERE ftype='DEPOSIT'),0)::text,
		       coalesce(sum(usd) FILTER (WHERE ftype='WITHDRAWAL'),0)::text,
		       coalesce(sum(usd),0)::text,
		       coalesce(max(usd),0)::text,
		       count(*) FILTER (WHERE usd IS NOT NULL AND usd < $3),
		       count(*) FILTER (WHERE usd IS NULL),
		       array_agg(id)
		  FROM (
		    SELECT id, account_id, type::text AS ftype,
		           CASE
		             WHEN usd_amount IS NOT NULL THEN usd_amount
		             WHEN currency = 'USD' THEN amount
		             ELSE NULL
		           END AS usd
		    FROM funding_transactions
		    WHERE type IN ('DEPOSIT','WITHDRAWAL')
		      AND status NOT IN ('FAILED','AUTO_CANCELLED')
		      AND created_at >= $1 AND created_at < $2
		  ) legs
		GROUP BY account_id
		HAVING coalesce(sum(usd),0) >= $3`, start, end, CTRThresholdUSD)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "ctr aggregation", err)
	}
	defer rows.Close()
	var aggs []dayAggregate
	for rows.Next() {
		var a dayAggregate
		var in, out, tot, mx *string
		var ids []int64
		if err := rows.Scan(&a.AccountID, &a.TxnCount, &in, &out, &tot,
			&mx, &a.SubCount, &a.UnpricedCount, &ids); err != nil {
			return 0, excerrors.Wrap("INTERNAL_ERROR", "ctr row scan", err)
		}
		a.CashInUSD = decOr(in)
		a.CashOutUSD = decOr(out)
		a.TotalUSD = decOr(tot)
		a.MaxSingleUSD = decOr(mx)
		a.TxIDs = ids
		aggs = append(aggs, a)
	}
	if err := rows.Err(); err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "ctr aggregation", err)
	}
	processed := 0
	for _, a := range aggs {
		if err := s.recordDay(ctx, a, start); err != nil {
			return processed, err
		}
		processed++
	}
	return processed, nil
}

// recordDay persists the CTR row + any monitoring detections for one
// account-day inside a single tx.
func (s *AMLService) recordDay(ctx context.Context, a dayAggregate,
	day time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "ctr tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	detail, _ := json.Marshal(map[string]any{
		"transaction_ids": a.TxIDs,
		"max_single_usd":  a.MaxSingleUSD.String(),
		"sub_threshold":   a.SubCount,
	})
	// Audit CTR_TRIGGERED on first insert only — a re-scan refreshes
	// the aggregates without re-firing the alert trail.
	var existed bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM ctr_reports
		              WHERE account_id = $1 AND business_date = $2)`,
		a.AccountID, day).Scan(&existed); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "ctr existence probe", err)
	}
	var ctrID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO ctr_reports
		    (account_id, business_date, txn_count, cash_in_usd,
		     cash_out_usd, total_usd, unpriced_count, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (account_id, business_date) DO UPDATE SET
		    txn_count      = EXCLUDED.txn_count,
		    cash_in_usd    = EXCLUDED.cash_in_usd,
		    cash_out_usd   = EXCLUDED.cash_out_usd,
		    total_usd      = EXCLUDED.total_usd,
		    unpriced_count = EXCLUDED.unpriced_count,
		    detail         = EXCLUDED.detail,
		    updated_at     = now()
		RETURNING id`,
		a.AccountID, day, a.TxnCount, a.CashInUSD, a.CashOutUSD,
		a.TotalUSD, a.UnpricedCount, detail).Scan(&ctrID)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "ctr upsert", err)
	}
	if !existed {
		if _, err := audit.Append(ctx, tx, "ctr_reports", &ctrID,
			"CTR_TRIGGERED", nil); err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "ctr audit", err)
		}
	}

	// Structuring / smurfing: multiple individually-sub-threshold legs
	// summing over the CTR floor is the deliberate-split signature.
	if a.SubCount >= 2 && a.MaxSingleUSD.LessThan(CTRThresholdUSD) {
		dedup := fmt.Sprintf("structuring:%d:%s", a.AccountID,
			day.Format("2006-01-02"))
		var evID int64
		err := tx.QueryRow(ctx, `
			INSERT INTO aml_monitoring_events
			    (rule_id, account_id, business_date, score, detail, dedup_key)
			VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (dedup_key) DO NOTHING
			RETURNING id`,
			AMLRuleStructuring, a.AccountID, day, ScoreStructuring,
			detail, dedup).Scan(&evID)
		if err == pgx.ErrNoRows {
			evID = 0 // duplicate — already recorded
		} else if err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "structuring event", err)
		}
		if evID != 0 {
			if _, err := audit.Append(ctx, tx, "aml_monitoring_events",
				&evID, "AML_RULE_HIT", nil); err != nil {
				return excerrors.Wrap("INTERNAL_ERROR", "aml event audit", err)
			}
		}
	}
	// CTR threshold breach also lands a low-weight monitoring event —
	// reportable volume feeds the account score without auto-filing.
	ctrDedup := fmt.Sprintf("ctr:%d:%s", a.AccountID, day.Format("2006-01-02"))
	var evID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO aml_monitoring_events
		    (rule_id, account_id, business_date, score, detail, dedup_key)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (dedup_key) DO NOTHING
		RETURNING id`,
		AMLRuleCTR, a.AccountID, day, ScoreCTRTrigger, detail, ctrDedup).Scan(&evID)
	if err != nil && err != pgx.ErrNoRows {
		return excerrors.Wrap("INTERNAL_ERROR", "ctr event", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "ctr commit", err)
	}

	// Post-commit: draft SARs for new detections + re-assess the account.
	if err := s.escalateDay(ctx, a, day); err != nil {
		return err
	}
	return nil
}

// escalateDay drafts the STRUCTURING SAR for a new detection and runs
// the rolling-score escalation — outside the record tx so a draft
// failure never rolls back the deterministic CTR/event rows (the dedup
// anchors make a later retry converge).
func (s *AMLService) escalateDay(ctx context.Context, a dayAggregate,
	day time.Time) error {
	if s.sar == nil {
		return nil
	}
	dedup := fmt.Sprintf("structuring:%d:%s", a.AccountID, day.Format("2006-01-02"))
	if a.SubCount >= 2 && a.MaxSingleUSD.LessThan(CTRThresholdUSD) {
		acct := a.AccountID
		rep, created, err := s.sar.Draft(ctx, SARDraftInput{
			TriggerType: SARTriggerStructuring,
			AccountID:   &acct,
			Description: fmt.Sprintf(
				"structuring pattern: %d sub-threshold transactions totalling %s USD on %s (max single %s)",
				a.SubCount, a.TotalUSD.String(), day.Format("2006-01-02"),
				a.MaxSingleUSD.String()),
			Evidence: map[string]any{
				"rule":          AMLRuleStructuring,
				"txn_count":     a.TxnCount,
				"sub_threshold": a.SubCount,
				"total_usd":     a.TotalUSD.String(),
			},
			TransactionIDs: a.TxIDs,
			SourceRef:      "aml:" + dedup,
			DetectedAt:     day,
		})
		if err != nil {
			return err
		}
		if created {
			// Link the monitoring event + CTR row back to the report.
			_, _ = s.pool.Exec(ctx, `
				UPDATE aml_monitoring_events SET sar_id = $2
				WHERE dedup_key = $1`, dedup, rep.ID)
			_, _ = s.pool.Exec(ctx, `
				UPDATE ctr_reports SET sar_id = $3
				WHERE account_id = $1 AND business_date = $2 AND sar_id IS NULL`,
				a.AccountID, day, rep.ID)
		}
	}
	return s.escalateScore(ctx, a.AccountID, day)
}

// escalateScore rolls up the trailing-90-day suspicion score; >= the
// EDD floor drafts a MONITORING_RULE SAR (dedup anchor per ISO week —
// a sustained high score re-reports weekly rather than once-ever or
// per-event) and upserts the account's CDD/EDD assessment.
func (s *AMLService) escalateScore(ctx context.Context, accountID int64,
	day time.Time) error {
	score, err := s.ScoreAccount(ctx, accountID)
	if err != nil {
		return err
	}
	level := "CDD"
	if score >= AMLScoreEDDFloor {
		level = "EDD"
	}
	reasons, _ := json.Marshal(map[string]any{
		"score": score, "window_days": 90, "floor": AMLScoreEDDFloor,
	})
	// The score column caps at 100 (migration 033 CHECK); the raw
	// rolling figure survives in reasons and the SAR description.
	stored := score
	if stored > 100 {
		stored = 100
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO aml_account_assessments (account_id, cdd_level, score, reasons)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (account_id) DO UPDATE SET
		    cdd_level = EXCLUDED.cdd_level, score = EXCLUDED.score,
		    reasons = EXCLUDED.reasons, assessed_at = now(),
		    updated_at = now()`, accountID, level, stored, reasons); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "aml assessment", err)
	}
	if score < AMLScoreEDDFloor || s.sar == nil {
		return nil
	}
	acct := accountID
	_, _, err = s.sar.Draft(ctx, SARDraftInput{
		TriggerType: SARTriggerMonitoring,
		AccountID:   &acct,
		Description: fmt.Sprintf(
			"rolling AML suspicion score %d >= %d — officer review required",
			score, AMLScoreEDDFloor),
		Evidence:   map[string]any{"score": score, "window": "90d"},
		SourceRef:  fmt.Sprintf("aml:score:%d:%s", accountID, isoWeekKey(day)),
		DetectedAt: day,
	})
	return err
}

// ScoreAccount sums monitoring-event scores over the trailing 90 days.
func (s *AMLService) ScoreAccount(ctx context.Context, accountID int64) (int, error) {
	var score *int
	err := s.pool.QueryRow(ctx, `
		SELECT coalesce(sum(score),0) FROM aml_monitoring_events
		WHERE account_id = $1 AND created_at > $2`,
		accountID, s.now().UTC().Add(-amlScoreWindow)).Scan(&score)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "aml score", err)
	}
	if score == nil {
		return 0, nil
	}
	return *score, nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// ListCTR returns newest-first CTR rows; status "" lists all.
func (s *AMLService) ListCTR(ctx context.Context, status string, limit int) ([]CTRReport, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, account_id, business_date, txn_count,
	             cash_in_usd::text, cash_out_usd::text, total_usd::text,
	             unpriced_count, status, sar_id, detail, created_at, updated_at
	      FROM ctr_reports`
	args := []any{}
	if status != "" {
		q += ` WHERE status = $1`
		args = append(args, status)
	}
	q += ` ORDER BY business_date DESC, id DESC LIMIT ` + fmt.Sprint(limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "ctr list", err)
	}
	defer rows.Close()
	var out []CTRReport
	for rows.Next() {
		var r CTRReport
		var in, out2, tot *string
		if err := rows.Scan(&r.ID, &r.AccountID, &r.BusinessDate,
			&r.TxnCount, &in, &out2, &tot, &r.UnpricedCount, &r.Status,
			&r.SARID, &r.Detail, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "ctr scan", err)
		}
		r.CashInUSD = decOr(in)
		r.CashOutUSD = decOr(out2)
		r.TotalUSD = decOr(tot)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListMonitoring returns newest-first rule detections.
func (s *AMLService) ListMonitoring(ctx context.Context, accountID int64,
	limit int) ([]MonitoringEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, rule_id, account_id, business_date, score, detail,
	             dedup_key, sar_id, created_at
	      FROM aml_monitoring_events`
	args := []any{}
	if accountID > 0 {
		q += ` WHERE account_id = $1`
		args = append(args, accountID)
	}
	q += ` ORDER BY id DESC LIMIT ` + fmt.Sprint(limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "monitoring list", err)
	}
	defer rows.Close()
	var out []MonitoringEvent
	for rows.Next() {
		var e MonitoringEvent
		if err := rows.Scan(&e.ID, &e.RuleID, &e.AccountID, &e.BusinessDate,
			&e.Score, &e.Detail, &e.DedupKey, &e.SARID, &e.CreatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "monitoring scan", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Program register — MSB Form 107, policies, training, officer, reviews
// ---------------------------------------------------------------------------

// ArtifactInput is the officer's registration request.
type ArtifactInput struct {
	ArtifactType string
	Reference    string // FinCEN filing/confirmation number
	Title        string
	Version      string
	Detail       map[string]any
	EffectiveAt  *time.Time
	ReviewDueAt  *time.Time
}

var validArtifacts = map[string]bool{
	ArtifactMSB: true, ArtifactRisk: true, ArtifactPolicy: true,
	ArtifactTraining: true, ArtifactOfficer: true, ArtifactAnnualReview: true,
}

// RegisterArtifact files one program-artifact row (officer-gated) and
// supersedes prior CURRENT rows of the same type+version scope.
func (s *AMLService) RegisterArtifact(ctx context.Context, officer int64,
	resolver HoldRoleResolver, in ArtifactInput) (*AMLArtifact, error) {
	if !validArtifacts[in.ArtifactType] {
		return nil, excerrors.New("INVALID_REQUEST",
			"unknown artifact_type "+in.ArtifactType)
	}
	if in.Title == "" {
		return nil, excerrors.New("INVALID_REQUEST", "artifact title required")
	}
	if resolver == nil {
		return nil, excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	role, err := resolver(ctx, officer)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if role != "Compliance Officer" && role != "Super Admin" {
		return nil, excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot register AML artifacts")
	}
	detail, _ := json.Marshal(in.Detail)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "artifact tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Same-type replacement marks the prior CURRENT row SUPERSEDED —
	// the register keeps version history, never deletes.
	if _, err := tx.Exec(ctx, `
		UPDATE aml_program_artifacts SET status = 'SUPERSEDED'
		WHERE artifact_type = $1 AND status = 'CURRENT'`,
		in.ArtifactType); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "artifact supersede", err)
	}
	var id int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO aml_program_artifacts
		    (artifact_type, reference, title, version, status, detail,
		     effective_at, review_due_at, recorded_by)
		VALUES ($1,NULLIF($2,''),$3,NULLIF($4,''),'CURRENT',$5,$6,$7,$8)
		RETURNING id`,
		in.ArtifactType, in.Reference, in.Title, in.Version, detail,
		in.EffectiveAt, in.ReviewDueAt, officer).Scan(&id); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "artifact insert", err)
	}
	if _, err := audit.Append(ctx, tx, "aml_program_artifacts", &id,
		"AML_ART_REG", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "artifact audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "artifact commit", err)
	}
	return &AMLArtifact{
		ID: id, ArtifactType: in.ArtifactType, Reference: in.Reference,
		Title: in.Title, Version: in.Version, Status: "CURRENT",
		Detail: detail, EffectiveAt: in.EffectiveAt,
		ReviewDueAt: in.ReviewDueAt, RecordedBy: &officer,
		CreatedAt: s.now().UTC(),
	}, nil
}

// ListArtifacts returns the register, newest-first; kind "" lists all.
func (s *AMLService) ListArtifacts(ctx context.Context, kind string,
	limit int) ([]AMLArtifact, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, artifact_type, reference, title, version, status,
	             detail, effective_at, review_due_at, recorded_by, created_at
	      FROM aml_program_artifacts`
	args := []any{}
	if kind != "" {
		q += ` WHERE artifact_type = $1`
		args = append(args, kind)
	}
	q += ` ORDER BY id DESC LIMIT ` + fmt.Sprint(limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "artifact list", err)
	}
	defer rows.Close()
	var out []AMLArtifact
	for rows.Next() {
		var a AMLArtifact
		var ref, ver *string
		if err := rows.Scan(&a.ID, &a.ArtifactType, &ref, &a.Title, &ver,
			&a.Status, &a.Detail, &a.EffectiveAt, &a.ReviewDueAt,
			&a.RecordedBy, &a.CreatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "artifact scan", err)
		}
		if ref != nil {
			a.Reference = *ref
		}
		if ver != nil {
			a.Version = *ver
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ProgramStatus evaluates the mandatory program artifacts and returns
// the breach list. A non-empty list means MSB_COMPLIANCE_BREACH —
// surfaced to ops (never a silent dashboard gap).
func (s *AMLService) ProgramStatus(ctx context.Context) (*AMLProgramStatus, error) {
	arts, err := s.ListArtifacts(ctx, "", 500)
	if err != nil {
		return nil, err
	}
	current := map[string]*AMLArtifact{}
	for i := range arts {
		a := arts[i]
		if a.Status == "CURRENT" {
			if _, ok := current[a.ArtifactType]; !ok {
				cp := a
				current[a.ArtifactType] = &cp
			}
		}
	}
	var breaches []string
	if current[ArtifactMSB] == nil {
		breaches = append(breaches, "no current MSB_REGISTRATION (FinCEN Form 107)")
	}
	if current[ArtifactOfficer] == nil {
		breaches = append(breaches, "no designated AML officer (OFFICER_DESIGNATION)")
	}
	if current[ArtifactPolicy] == nil {
		breaches = append(breaches, "no current AML POLICY version")
	}
	if current[ArtifactRisk] == nil {
		breaches = append(breaches, "no current RISK_ASSESSMENT")
	}
	now := s.now().UTC()
	if rv := current[ArtifactAnnualReview]; rv == nil {
		breaches = append(breaches, "no ANNUAL_REVIEW record")
	} else if rv.ReviewDueAt != nil && rv.ReviewDueAt.Before(now) {
		breaches = append(breaches, fmt.Sprintf(
			"annual review overdue since %s", rv.ReviewDueAt.Format("2006-01-02")))
	}
	st := &AMLProgramStatus{Compliant: len(breaches) == 0, Breaches: breaches}
	if !st.Compliant {
		st.Code = "MSB_COMPLIANCE_BREACH"
		if s.alerter != nil {
			_ = s.alerter.RaiseHold(ctx, HoldAlert{
				Severity: "P1", Code: "MSB_COMPLIANCE_BREACH",
				AccountID: 0,
				Summary:   fmt.Sprintf("AML program breach: %v", breaches),
			})
		}
	}
	return st, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func decOr(s *string) decimal.Decimal {
	if s == nil {
		return decimal.Zero
	}
	return decimal.RequireFromString(*s)
}

// isoWeekKey renders the deterministic per-account weekly dedup anchor
// for score-escalation drafts.
func isoWeekKey(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}
