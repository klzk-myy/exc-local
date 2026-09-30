// model_validation.go — Phase-19 Tasks 19.3.13 §5 + 19.3.21: margin
// model validation register, parameter change control, validator
// independence, through-the-cycle floors, concentration/liquidity
// add-ons, insurance-fund calibration and the intraday client-money
// guard (spec §13.10, §13.12, §24 #206/#344).
//
// The stress/backtest machinery itself lives in stress_engine.go; this
// file owns the persistence seam they share (margin_model_runs,
// migration 064) plus everything a model-risk supervisor asks about
// first (§13.12):
//
//   - ParamChangeGate: a margin/liquidation parameter change is gated
//     on (a) dual control — owner ≠ approver — and (b) a linked PASS
//     validation run whose recorded validator (reviewed_by) differs
//     from the change owner. Any failure rejects with
//     MARGIN_MODEL_UNVALIDATED (§23, HTTP 503).
//   - Through-the-cycle floors + anti-procyclicality caps: proposed
//     IM rates clamp into [TTCFloor, baseline × cap] so a volatility
//     collapse cannot procyclically starve margin below the cycle
//     floor nor a spike blow it out beyond the cap.
//   - Concentration/liquidity add-ons beyond the §13.6f tiered-leverage
//     bands: a position dominating instrument OI or exceeding one day
//     of average volume pays add-on margin.
//   - Insurance calibration: the §13.6/Task-19.3.14 target (0.5% of
//     client equity, $10M floor) is tied at inception to the stress
//     suite's worst-1% adequacy metric.
//   - Intraday client-money guard: 105% over-segregation target,
//     1-hour client→house margin settlement rule, and per-currency
//     negative-interest disclosure.
//
// Fail-closed (§2.7): every store read that cannot decode fails the
// operation — a gate that cannot prove validation must not pass.
package risk

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Run kinds + outcomes — margin_model_runs (migration 064).
const (
	RunKindStress   = "STRESS"
	RunKindBacktest = "BACKTEST"

	RunStatusPass = "PASS"
	RunStatusFail = "FAIL"
)

// Change-control + guard codes. MARGIN_MODEL_UNVALIDATED and
// DUAL_CONTROL_REQUIRED are §23-registered (503/400); the remainder are
// internal-only operational alerts (§23 internal-only list) routed to
// the Risk Manager review queue via the OpsAlerter seam.
const (
	CodeMarginModelUnvalidated = "MARGIN_MODEL_UNVALIDATED"     // §23 503
	codeDualControlRequired    = "DUAL_CONTROL_REQUIRED"        // §23 400
	codeMarginModelAdequacy    = "MARGIN_MODEL_ADEQUACY_BREACH" // internal P1 — fund < worst-1% shortfall
	codeBacktestBreach         = "MARGIN_BACKTEST_BREACH"       // internal P1 — realized slippage beyond predicted floor
	codeClientMoneyShortfall   = "CLIENT_MONEY_SEG_SHORTFALL"   // internal P1 — segregated < required×1.05
	codeMarginTransferLate     = "MARGIN_TRANSFER_LATE"         // internal P1 — client→house margin unsettled >1h
)

// ModelRun is one margin_model_runs row.
type ModelRun struct {
	RunID         int64
	Kind          string // STRESS | BACKTEST
	Scenario      string
	Status        string // PASS | FAIL
	ResultMetrics json.RawMessage
	BreachCount   int
	ParamChange   *string // parameter name the run validates (nil = scheduled sweep)
	InitiatedBy   *int64  // nil = scheduler
	ReviewedBy    *int64  // independent validator identity
	ReviewedAt    *time.Time
	CreatedAt     time.Time
}

// ModelRunStore is the persistence seam; PgModelRunStore implements it
// over pgx — tests substitute fakes.
type ModelRunStore interface {
	// InsertRun persists one run and returns its run_id.
	InsertRun(ctx context.Context, r ModelRun) (int64, error)
	// RunByID resolves one run; (nil, nil) when absent.
	RunByID(ctx context.Context, runID int64) (*ModelRun, error)
	// LatestRun returns the newest run of a kind; (nil, nil) when the
	// kind has never run (first-run edge).
	LatestRun(ctx context.Context, kind string) (*ModelRun, error)
	// SumBreaches totals breach_count over runs of a kind since a
	// timestamp — the Basel-style rolling-window count.
	SumBreaches(ctx context.Context, kind string, since time.Time) (int, error)
	// RecordReview pins the independent validator on the run. A run
	// already carrying a different reviewer is an error — validator
	// identity is evidence, not a mutable field.
	RecordReview(ctx context.Context, runID, reviewerID int64) error
	// ListRuns returns runs of a kind since a timestamp (quarterly
	// re-validation report input), oldest first.
	ListRuns(ctx context.Context, kind string, since time.Time) ([]ModelRun, error)
}

// PgModelRunStore implements ModelRunStore over pgx.
type PgModelRunStore struct{ pool *pgxpool.Pool }

// NewPgModelRunStore binds the pool — nil is rejected fail-closed.
func NewPgModelRunStore(pool *pgxpool.Pool) (*PgModelRunStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("model validation store: nil pgx pool")
	}
	return &PgModelRunStore{pool: pool}, nil
}

// InsertRun implements ModelRunStore. metrics nil → '{}'; empty status
// → PASS is NOT assumed — the caller must state the outcome.
func (s *PgModelRunStore) InsertRun(ctx context.Context, r ModelRun) (int64, error) {
	if r.Status != RunStatusPass && r.Status != RunStatusFail {
		return 0, excerrors.New(CodeInvalidRequest,
			fmt.Sprintf("model run: invalid status %q — want PASS|FAIL", r.Status))
	}
	if r.Kind != RunKindStress && r.Kind != RunKindBacktest {
		return 0, excerrors.New(CodeInvalidRequest,
			fmt.Sprintf("model run: invalid kind %q — want STRESS|BACKTEST", r.Kind))
	}
	metrics := "{}"
	if len(r.ResultMetrics) > 0 {
		metrics = string(r.ResultMetrics)
	}
	param := ""
	if r.ParamChange != nil {
		param = *r.ParamChange
	}
	var initiator int64
	if r.InitiatedBy != nil {
		initiator = *r.InitiatedBy
	}
	var created *time.Time
	if !r.CreatedAt.IsZero() {
		created = &r.CreatedAt
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO margin_model_runs
		    (kind, scenario, status, result_metrics, breach_count,
		     param_change, initiated_by, reviewed_by, created_at)
		VALUES ($1,$2,$3,$4::jsonb,$5,NULLIF($6,''),NULLIF($7::bigint,0),$8,
		        COALESCE($9::timestamptz, now()))
		RETURNING run_id`,
		r.Kind, r.Scenario, r.Status, metrics, r.BreachCount,
		param, initiator, r.ReviewedBy, created).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("model run insert %s/%s: %w", r.Kind, r.Scenario, err)
	}
	return id, nil
}

const modelRunCols = `run_id, kind, scenario, status, result_metrics,
	breach_count, param_change, initiated_by, reviewed_by, reviewed_at, created_at`

func scanModelRun(row pgx.Row) (*ModelRun, error) {
	var r ModelRun
	if err := row.Scan(&r.RunID, &r.Kind, &r.Scenario, &r.Status,
		&r.ResultMetrics, &r.BreachCount, &r.ParamChange, &r.InitiatedBy,
		&r.ReviewedBy, &r.ReviewedAt, &r.CreatedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

// RunByID implements ModelRunStore.
func (s *PgModelRunStore) RunByID(ctx context.Context, runID int64) (*ModelRun, error) {
	r, err := scanModelRun(s.pool.QueryRow(ctx,
		`SELECT `+modelRunCols+` FROM margin_model_runs WHERE run_id = $1`, runID))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("model run %d: %w", runID, err)
	}
	return r, nil
}

// LatestRun implements ModelRunStore.
func (s *PgModelRunStore) LatestRun(ctx context.Context, kind string) (*ModelRun, error) {
	r, err := scanModelRun(s.pool.QueryRow(ctx, `
		SELECT `+modelRunCols+` FROM margin_model_runs
		WHERE kind = $1 ORDER BY created_at DESC, run_id DESC LIMIT 1`, kind))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest %s run: %w", kind, err)
	}
	return r, nil
}

// SumBreaches implements ModelRunStore.
func (s *PgModelRunStore) SumBreaches(ctx context.Context, kind string, since time.Time) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(breach_count), 0) FROM margin_model_runs
		WHERE kind = $1 AND created_at >= $2`, kind, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("breach sum %s: %w", kind, err)
	}
	return n, nil
}

// RecordReview implements ModelRunStore — the update only lands while
// reviewed_by is NULL; a second/different reviewer is refused so the
// independence record cannot be rewritten after the fact.
func (s *PgModelRunStore) RecordReview(ctx context.Context, runID, reviewerID int64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE margin_model_runs SET reviewed_by = $2, reviewed_at = now()
		WHERE run_id = $1 AND reviewed_by IS NULL`, runID, reviewerID)
	if err != nil {
		return fmt.Errorf("model run %d review: %w", runID, err)
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New(codeDualControlRequired,
			fmt.Sprintf("model run %d already reviewed or missing — validator identity is immutable", runID))
	}
	return nil
}

// ListRuns implements ModelRunStore.
func (s *PgModelRunStore) ListRuns(ctx context.Context, kind string, since time.Time) ([]ModelRun, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+modelRunCols+` FROM margin_model_runs
		WHERE kind = $1 AND created_at >= $2 ORDER BY created_at, run_id`, kind, since)
	if err != nil {
		return nil, fmt.Errorf("model run list %s: %w", kind, err)
	}
	defer rows.Close()
	var out []ModelRun
	for rows.Next() {
		r, err := scanModelRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Parameter change control (Task 19.3.13 §5 + Task 19.3.21 §1)
// ---------------------------------------------------------------------------

// ParamChange is a proposed margin/liquidation parameter mutation
// (auction floor factor, EXTEND decay step, a leverage-tier band, a
// margin floor, …). OwnerID is the change author; ApprovedBy the
// dual-control second principal; RunID the linked validation run.
type ParamChange struct {
	ChangeID   int64           `json:"change_id,omitempty"`
	Parameter  string          `json:"parameter"`
	Proposed   json.RawMessage `json:"proposed_value"`
	OwnerID    int64           `json:"owner_id"`
	ApprovedBy int64           `json:"approved_by"`
	RunID      int64           `json:"run_id"`
}

// ParamChangeStore persists gated changes (margin_model_param_changes).
type ParamChangeStore interface {
	InsertChange(ctx context.Context, c ParamChange) (int64, error)
	// DecideChange stamps VALIDATED/REJECTED/APPLIED + decided_at.
	DecideChange(ctx context.Context, changeID int64, status, reason string) error
}

// PgParamChangeStore implements ParamChangeStore over pgx.
type PgParamChangeStore struct{ pool *pgxpool.Pool }

// NewPgParamChangeStore binds the pool — nil rejected fail-closed.
func NewPgParamChangeStore(pool *pgxpool.Pool) (*PgParamChangeStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("param change store: nil pgx pool")
	}
	return &PgParamChangeStore{pool: pool}, nil
}

// InsertChange implements ParamChangeStore — the dual-control CHECK on
// the table is the last line of defense; the gate refuses
// approved==owner before ever reaching it.
func (s *PgParamChangeStore) InsertChange(ctx context.Context, c ParamChange) (int64, error) {
	proposed := "{}"
	if len(c.Proposed) > 0 {
		proposed = string(c.Proposed)
	}
	var approved *int64
	if c.ApprovedBy != 0 {
		approved = &c.ApprovedBy
	}
	var run *int64
	if c.RunID != 0 {
		run = &c.RunID
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO margin_model_param_changes
		    (parameter, proposed_value, owner_id, approved_by, run_id)
		VALUES ($1,$2::jsonb,$3,$4,$5) RETURNING id`,
		c.Parameter, proposed, c.OwnerID, approved, run).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("param change insert %s: %w", c.Parameter, err)
	}
	return id, nil
}

// DecideChange implements ParamChangeStore.
func (s *PgParamChangeStore) DecideChange(ctx context.Context, changeID int64, status, reason string) error {
	var r *string
	if reason != "" {
		r = &reason
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE margin_model_param_changes
		SET status = $2, reject_reason = $3, decided_at = now()
		WHERE id = $1`, changeID, status, r)
	if err != nil {
		return fmt.Errorf("param change %d decide %s: %w", changeID, status, err)
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New("NOT_FOUND", fmt.Sprintf("param change %d missing", changeID))
	}
	return nil
}

// Param-change row status vocabulary (migration 064 CHECK).
const (
	ChangeStatusPending   = "PENDING"
	ChangeStatusValidated = "VALIDATED"
	ChangeStatusRejected  = "REJECTED"
	ChangeStatusApplied   = "APPLIED"
)

// DefaultMaxRunAge is the freshness bound on a linked validation run —
// a run older than one quarter cannot prove today's change (the
// quarterly re-validation cadence is the supervisor contract).
const DefaultMaxRunAge = 90 * 24 * time.Hour

// ParamChangeGate evaluates ParamChanges against the §13.12 contract.
// Admin surfaces call ValidateParameterChange before mutating any
// margin/liquidation parameter; GateAndRecord is the audit-recording
// variant that persists the PENDING → VALIDATED|REJECTED transition.
type ParamChangeGate struct {
	runs    ModelRunStore
	changes ParamChangeStore // optional — GateAndRecord only
	maxAge  time.Duration
	now     func() time.Time
}

// NewParamChangeGate binds the run store. maxAge ≤ 0 takes
// DefaultMaxRunAge; now nil takes UTC wall clock.
func NewParamChangeGate(runs ModelRunStore, changes ParamChangeStore,
	maxAge time.Duration, now func() time.Time) (*ParamChangeGate, error) {

	if runs == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "param change gate: nil run store")
	}
	if maxAge <= 0 {
		maxAge = DefaultMaxRunAge
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &ParamChangeGate{runs: runs, changes: changes, maxAge: maxAge, now: now}, nil
}

// ValidateParameterChange implements the §13.12 gate:
//
//  1. Dual control — the change needs a second principal distinct from
//     the owner (DUAL_CONTROL_REQUIRED, §8.2).
//  2. Linked run — RunID must resolve to a margin_model_runs row.
//  3. Passing — the run status must be PASS.
//  4. Fresh — the run must be within maxAge (stale proof is no proof).
//  5. Independence — the run's recorded validator (reviewed_by) must
//     exist and differ from the change owner.
//  6. Scope — when the run was executed for a named parameter, it must
//     match the change's parameter (a EURUSD-tier run cannot validate
//     an auction-floor change).
//
// Any failure past step 1 rejects MARGIN_MODEL_UNVALIDATED (§23 503) —
// the admin surface surfaces it as a service-level refusal, never a
// silent pass.
func (g *ParamChangeGate) ValidateParameterChange(ctx context.Context, c ParamChange) error {
	if c.Parameter == "" || c.OwnerID <= 0 {
		return excerrors.New(CodeInvalidRequest,
			"param change: parameter and owner_id are required")
	}
	if c.ApprovedBy == 0 || c.ApprovedBy == c.OwnerID {
		return excerrors.New(codeDualControlRequired,
			"param change: requires a second-authorizer approval distinct from the owner")
	}
	if c.RunID == 0 {
		return excerrors.New(CodeMarginModelUnvalidated,
			"param change: no linked validation run")
	}
	run, err := g.runs.RunByID(ctx, c.RunID)
	if err != nil {
		return excerrors.Wrap(CodeMarginModelUnvalidated, "param change: run lookup", err)
	}
	if run == nil {
		return excerrors.New(CodeMarginModelUnvalidated,
			fmt.Sprintf("param change: validation run %d not found", c.RunID))
	}
	if run.Status != RunStatusPass {
		return excerrors.New(CodeMarginModelUnvalidated,
			fmt.Sprintf("param change: linked run %d is %s, not PASS", run.RunID, run.Status))
	}
	if g.now().Sub(run.CreatedAt) > g.maxAge {
		return excerrors.New(CodeMarginModelUnvalidated,
			fmt.Sprintf("param change: run %d is stale (created %s, max age %s)",
				run.RunID, run.CreatedAt.Format(time.RFC3339), g.maxAge))
	}
	if run.ReviewedBy == nil || *run.ReviewedBy == 0 {
		return excerrors.New(CodeMarginModelUnvalidated,
			fmt.Sprintf("param change: run %d has no recorded validator — independence unprovable", run.RunID))
	}
	if *run.ReviewedBy == c.OwnerID {
		return excerrors.New(CodeMarginModelUnvalidated,
			fmt.Sprintf("param change: validator %d is the change owner — independence violated", c.OwnerID))
	}
	if run.ParamChange != nil && *run.ParamChange != "" && *run.ParamChange != c.Parameter {
		return excerrors.New(CodeMarginModelUnvalidated,
			fmt.Sprintf("param change: run %d validates %q, not %q",
				run.RunID, *run.ParamChange, c.Parameter))
	}
	return nil
}

// GateAndRecord persists the change PENDING, evaluates it, then stamps
// VALIDATED or REJECTED — the §13.12 audit trail of every proposal,
// passing or refused. A nil changes store degrades to validate-only
// (the caller loses the audit row — construction-time wiring is the
// parent's responsibility).
func (g *ParamChangeGate) GateAndRecord(ctx context.Context, c ParamChange) (int64, error) {
	if g.changes == nil {
		return 0, g.ValidateParameterChange(ctx, c)
	}
	id, err := g.changes.InsertChange(ctx, c)
	if err != nil {
		return 0, err
	}
	if verr := g.ValidateParameterChange(ctx, c); verr != nil {
		reason := verr.Error()
		if len(reason) > 255 {
			reason = reason[:255]
		}
		if derr := g.changes.DecideChange(ctx, id, ChangeStatusRejected, reason); derr != nil {
			return id, excerrors.Wrap(CodeRiskLimitsInternal, "param change reject record", derr)
		}
		return id, verr
	}
	if err := g.changes.DecideChange(ctx, id, ChangeStatusValidated, ""); err != nil {
		return id, excerrors.Wrap(CodeRiskLimitsInternal, "param change validate record", err)
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// Through-the-cycle floors + anti-procyclicality caps (Task 19.3.21 §1)
// ---------------------------------------------------------------------------

var (
	// DefaultTTCMarginFloorRate is the through-the-cycle IM floor: a
	// proposed initial-margin rate may never drop below this fraction
	// of notional regardless of how benign realized volatility looks.
	// 0.5% ≈ the ESMA 30:1 major-pair retail floor's professional
	// analogue — the Risk Manager owns the calibration.
	DefaultTTCMarginFloorRate = decimal.RequireFromString("0.005")
	// DefaultAntiProcycCap is the maximum ratio a proposed IM rate may
	// reach relative to its through-the-cycle baseline (125%).
	DefaultAntiProcycCap = decimal.RequireFromString("1.25")
)

// ThroughTheCycleClamp pins a proposed IM rate into
// [floor, baseline × cap]. The returned flags report which bound bound
// (if any) so the admin surface can disclose the intervention rather
// than silently rewriting the proposal.
func ThroughTheCycleClamp(proposedRate, ttcBaselineRate decimal.Decimal) (
	clamped decimal.Decimal, hitFloor, hitCap bool) {

	clamped = proposedRate
	if clamped.LessThan(DefaultTTCMarginFloorRate) {
		clamped, hitFloor = DefaultTTCMarginFloorRate, true
	}
	cap_ := ttcBaselineRate.Mul(DefaultAntiProcycCap)
	if ttcBaselineRate.IsPositive() && clamped.GreaterThan(cap_) {
		clamped, hitCap = cap_, true
	}
	return clamped, hitFloor, hitCap
}

// ---------------------------------------------------------------------------
// Concentration & liquidity add-ons (Task 19.3.21 §1 — beyond the
// §13.6f tiered-leverage bands)
// ---------------------------------------------------------------------------

var (
	// ConcentrationShareThreshold — a position above 25% of instrument
	// OI is "concentrated" (the venue's own liquidation flow would move
	// the market against itself).
	ConcentrationShareThreshold = decimal.RequireFromString("0.25")
	// ConcentrationAddonSlope — add-on = base margin × excess-share ×
	// 0.5 (a 50%-of-OI position pays 12.5% on top of banded margin).
	ConcentrationAddonSlope = decimal.RequireFromString("0.5")
	// LiquidityAddonPerDay — positions needing >1 day of average daily
	// volume to unwind pay 10% of base margin per excess day…
	LiquidityAddonPerDay = decimal.RequireFromString("0.10")
	// LiquidityAddonCap — …up to a 50% total add-on.
	LiquidityAddonCap = decimal.RequireFromString("0.50")
)

// ConcentrationAddon returns the extra margin a dominant position
// pays: baseMargin × max(0, share−0.25) × 0.5 where share =
// positionNotional / instrumentOI. An unknown/degenerate OI with a
// positive position is treated as fully concentrated (share = 1) —
// pessimism over precision (§2.7).
func ConcentrationAddon(positionNotional, instrumentOI, baseMargin decimal.Decimal) decimal.Decimal {
	if !positionNotional.IsPositive() || !baseMargin.IsPositive() {
		return decimal.Zero
	}
	share := decimal.One
	if instrumentOI.IsPositive() {
		share = positionNotional.Div(instrumentOI)
		if share.GreaterThan(decimal.One) {
			share = decimal.One
		}
	}
	excess := share.Sub(ConcentrationShareThreshold)
	if !excess.IsPositive() {
		return decimal.Zero
	}
	return baseMargin.Mul(excess).Mul(ConcentrationAddonSlope).Round(8)
}

// LiquidityAddon returns the extra margin a slow-to-unwind position
// pays: days = positionNotional / advNotional; days ≤ 1 → 0; otherwise
// baseMargin × min((days−1) × 0.10, 0.50). An unknown ADV is the worst
// case — the cap applies (§2.7 pessimism).
func LiquidityAddon(positionNotional, advNotional, baseMargin decimal.Decimal) decimal.Decimal {
	if !positionNotional.IsPositive() || !baseMargin.IsPositive() {
		return decimal.Zero
	}
	if !advNotional.IsPositive() {
		return baseMargin.Mul(LiquidityAddonCap).Round(8)
	}
	days := positionNotional.Div(advNotional)
	excess := days.Sub(decimal.One)
	if !excess.IsPositive() {
		return decimal.Zero
	}
	addon := excess.Mul(LiquidityAddonPerDay)
	if addon.GreaterThan(LiquidityAddonCap) {
		addon = LiquidityAddonCap
	}
	return baseMargin.Mul(addon).Round(8)
}

// ---------------------------------------------------------------------------
// Insurance-fund calibration & segregated custody (Task 19.3.21 §2)
// ---------------------------------------------------------------------------

var (
	// InsuranceEquityFraction is the Task-19.3.14 target leg: 0.5% of
	// total client equity (mirrors insurance_fund_governance defaults).
	InsuranceEquityFraction = decimal.RequireFromString("0.005")
	// InsuranceMinTargetUSD is the §13.6 $10M minimum capitalization.
	InsuranceMinTargetUSD = decimal.NewFromInt(10_000_000)
)

// WorstCaseShortfallSource is the calibration seam the insurance-fund
// service binds (parent's wiring — insurance_fund.go is read-only for
// this task). StressEngine implements it: WorstOnePctShortfall of the
// latest suite is the stress-metric leg of the fund target.
type WorstCaseShortfallSource interface {
	WorstOnePctShortfall(ctx context.Context) (decimal.Decimal, error)
}

// CalibratedFundTarget is the §13.12 fund target at inception:
//
//	max(0.5% × clientEquityUSD, $10M, worstOnePctShortfall)
//
// — the Task-19.3.14 formula plus the stress-metric adequacy leg.
func CalibratedFundTarget(clientEquityUSD, worstOnePctShortfall decimal.Decimal) decimal.Decimal {
	target := clientEquityUSD.Mul(InsuranceEquityFraction)
	if target.LessThan(InsuranceMinTargetUSD) {
		target = InsuranceMinTargetUSD
	}
	if target.LessThan(worstOnePctShortfall) {
		target = worstOnePctShortfall
	}
	return target.Round(8)
}

// SegregatedCustody describes where one currency's fund cash actually
// sits. Fund cash must be held in segregated nostro custody under the
// restricted house-asset GL (1150_INSURANCE_FUND_NOSTRO_{CCY}) —
// distinct from the house operating nostro (1010_NOSTRO_{CCY}) and
// reconciled by the Phase-24 ledger work.
type SegregatedCustody struct {
	Currency           string `json:"currency"`
	GLAccount          string `json:"gl_account"`           // ledger code the cash posts to
	ExternalAccountRef string `json:"external_account_ref"` // bank/nostro account reference
	Segregated         bool   `json:"segregated"`           // legally segregated from house ops
}

// VerifySegregatedCustody proves a custody descriptor is what it
// claims: flagged segregated, and posted to the canonical 1150 fund
// nostro GL for the currency — never the 1010 house operating nostro.
func VerifySegregatedCustody(c SegregatedCustody) error {
	if len(c.Currency) != 3 {
		return excerrors.New(CodeInvalidRequest,
			fmt.Sprintf("custody: currency %q must be 3-letter ISO", c.Currency))
	}
	if !c.Segregated {
		return excerrors.New(codeClientMoneyShortfall,
			fmt.Sprintf("custody: %s fund cash is not held segregated", c.Currency))
	}
	want := ledger.InsuranceFundNostro(c.Currency)
	if c.GLAccount != want {
		return excerrors.New(codeClientMoneyShortfall,
			fmt.Sprintf("custody: %s fund cash posts to %q, want segregated nostro %q",
				c.Currency, c.GLAccount, want))
	}
	return nil
}

// FundSegment is one currency's calibration view: balance, target and
// the custody descriptor — the per-settlement-currency segmentation
// the Task-19.3.14 governance table materializes.
type FundSegment struct {
	Currency string            `json:"currency"`
	Balance  string            `json:"balance"`
	Target   string            `json:"target"`
	Custody  SegregatedCustody `json:"custody"`
	Adequate bool              `json:"adequate"` // balance ≥ target
}

// FundSegmentReport is the segmented reconciliation view the Risk
// Manager reviews (one row per settlement currency).
type FundSegmentReport struct {
	Segments []FundSegment `json:"segments"`
	Ts       time.Time     `json:"ts"`
}

// ---------------------------------------------------------------------------
// Intraday client-money guard (Task 19.3.21 §3)
// ---------------------------------------------------------------------------

var (
	// ClientMoneyBufferTarget is the 105% over-segregation target.
	ClientMoneyBufferTarget = decimal.RequireFromString("1.05")
	// MarginTransferSettlementLimit is the client→house margin
	// transfer timing rule: settles within the hour.
	MarginTransferSettlementLimit = time.Hour
)

// PendingMarginTransfer is an unsettled client→house margin move.
type PendingMarginTransfer struct {
	ID          int64           `json:"id"`
	Amount      decimal.Decimal `json:"amount"`
	RequestedAt time.Time       `json:"requested_at"`
}

// ClientMoneySnapshot is one currency's intraday segregation state —
// produced by the Phase-24 shortfall calculation (Task 24.3.16 seam),
// consumed here.
type ClientMoneySnapshot struct {
	Currency               string
	SegregatedHeld         decimal.Decimal         // 1110 segregated custody balance
	RequiredSegregation    decimal.Decimal         // client equity + locked margin owed
	PendingMarginTransfers []PendingMarginTransfer // client→house margin in flight
}

// ClientMoneyReport is the guard's evaluated row — emitted to the sink
// and paged on breach. Decimal fields serialize as strings.
type ClientMoneyReport struct {
	Currency           string    `json:"currency"`
	Required           string    `json:"required"`
	Held               string    `json:"held"`
	RequiredWithBuffer string    `json:"required_with_buffer"` // required × 1.05
	Shortfall          string    `json:"shortfall"`            // max(0, requiredWithBuffer − held)
	Ratio              string    `json:"ratio"`                // held / required (∞ as "" when required=0)
	HardBreach         bool      `json:"hard_breach"`          // held < required
	BufferBreach       bool      `json:"buffer_breach"`        // held < required×1.05
	LateTransfers      []int64   `json:"late_transfers"`       // pending > 1h
	Ts                 time.Time `json:"ts"`
}

// ClientMoneySink receives evaluated reports — production binds the
// Phase-24 persistence surface; tests capture in-memory.
type ClientMoneySink interface {
	RecordReport(ctx context.Context, r ClientMoneyReport) error
}

// ClientMoneySource supplies the intraday snapshots (Phase-24 Task
// 24.3.16 real-time shortfall calculation in production).
type ClientMoneySource interface {
	Snapshot(ctx context.Context) ([]ClientMoneySnapshot, error)
}

// ClientMoneyGuard evaluates one snapshot: computes the 105% buffer
// shortfall, the >1h unsettled margin transfers, emits the report row
// and raises the Risk Manager alert on breach.
type ClientMoneyGuard struct {
	alerter OpsAlerter
	sink    ClientMoneySink
	now     func() time.Time
	logf    func(format string, args ...any)
}

// NewClientMoneyGuard wires the guard; alerter and sink may be nil in
// tests (alerts/reports are logged and dropped, never silently masked
// in production wiring).
func NewClientMoneyGuard(alerter OpsAlerter, sink ClientMoneySink,
	now func() time.Time, logf func(string, ...any)) *ClientMoneyGuard {

	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &ClientMoneyGuard{alerter: alerter, sink: sink, now: now, logf: logf}
}

// Evaluate computes the report for one currency snapshot.
func (g *ClientMoneyGuard) Evaluate(ctx context.Context, s ClientMoneySnapshot) (ClientMoneyReport, error) {
	if len(s.Currency) != 3 {
		return ClientMoneyReport{}, excerrors.New(CodeInvalidRequest,
			fmt.Sprintf("client-money guard: currency %q must be 3-letter ISO", s.Currency))
	}
	if s.SegregatedHeld.IsNegative() || s.RequiredSegregation.IsNegative() {
		return ClientMoneyReport{}, excerrors.New(CodeInvalidRequest,
			"client-money guard: negative held/required input")
	}
	now := g.now()
	withBuffer := s.RequiredSegregation.Mul(ClientMoneyBufferTarget).Round(8)
	shortfall := withBuffer.Sub(s.SegregatedHeld)
	if shortfall.IsNegative() {
		shortfall = decimal.Zero
	}
	r := ClientMoneyReport{
		Currency:           s.Currency,
		Required:           s.RequiredSegregation.String(),
		Held:               s.SegregatedHeld.String(),
		RequiredWithBuffer: withBuffer.String(),
		Shortfall:          shortfall.String(),
		HardBreach:         s.RequiredSegregation.IsPositive() && s.SegregatedHeld.LessThan(s.RequiredSegregation),
		BufferBreach:       s.SegregatedHeld.LessThan(withBuffer),
		Ts:                 now,
	}
	if s.RequiredSegregation.IsPositive() {
		r.Ratio = s.SegregatedHeld.Div(s.RequiredSegregation).Round(6).String()
	}
	for _, t := range s.PendingMarginTransfers {
		if now.Sub(t.RequestedAt) > MarginTransferSettlementLimit {
			r.LateTransfers = append(r.LateTransfers, t.ID)
		}
	}
	if g.sink != nil {
		if err := g.sink.RecordReport(ctx, r); err != nil {
			g.logf("client-money guard: report sink %s: %v", s.Currency, err)
		}
	}
	// Breach routing — a hard breach and a bare buffer miss are both P1
	// (a hard breach is not P0: the fund, not client money, is the
	// first loss layer; the page is the Risk Manager review case).
	if r.HardBreach || r.BufferBreach {
		g.raise(ctx, codeClientMoneyShortfall, fmt.Sprintf(
			"client money %s: segregated %s < required-with-buffer %s (shortfall %s, hard=%v)",
			r.Currency, r.Held, r.RequiredWithBuffer, r.Shortfall, r.HardBreach),
			map[string]string{"currency": r.Currency, "held": r.Held,
				"required_with_buffer": r.RequiredWithBuffer, "shortfall": r.Shortfall})
	}
	for _, id := range r.LateTransfers {
		g.raise(ctx, codeMarginTransferLate, fmt.Sprintf(
			"client money %s: margin transfer %d unsettled beyond the 1h rule",
			r.Currency, id), map[string]string{
			"currency": r.Currency, "transfer_id": fmt.Sprint(id)})
	}
	return r, nil
}

func (g *ClientMoneyGuard) raise(ctx context.Context, code, summary string, details map[string]string) {
	if g.alerter == nil {
		g.logf("client-money guard: alert %s undeliverable (no alerter): %s", code, summary)
		return
	}
	actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.alerter.Raise(actx, OpsAlert{
		Severity: SeverityP1, Code: code, Summary: summary, Details: details,
	}); err != nil {
		g.logf("client-money guard: alert %s dispatch failed: %v", code, err)
	}
}

// IntradayBufferMonitor is the periodic driver: each tick it pulls the
// Phase-24 shortfall snapshots and evaluates every currency through
// the guard — the "real-time" half of the client-money guard pair.
type IntradayBufferMonitor struct {
	guard  *ClientMoneyGuard
	source ClientMoneySource
}

// NewIntradayBufferMonitor binds guard + source; nil source is
// rejected (a monitor with nothing to monitor is a wiring defect).
func NewIntradayBufferMonitor(g *ClientMoneyGuard, source ClientMoneySource) (*IntradayBufferMonitor, error) {
	if g == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "intraday monitor: nil guard")
	}
	if source == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "intraday monitor: nil source")
	}
	return &IntradayBufferMonitor{guard: g, source: source}, nil
}

// TickOnce evaluates one intraday pass — the scheduler entry point the
// parent binds to its ticker.
func (m *IntradayBufferMonitor) TickOnce(ctx context.Context) ([]ClientMoneyReport, error) {
	snaps, err := m.source.Snapshot(ctx)
	if err != nil {
		return nil, excerrors.Wrap(CodeRiskLimitsInternal, "intraday snapshot", err)
	}
	reports := make([]ClientMoneyReport, 0, len(snaps))
	for _, s := range snaps {
		r, err := m.guard.Evaluate(ctx, s)
		if err != nil {
			return reports, err
		}
		reports = append(reports, r)
	}
	return reports, nil
}

// ---------------------------------------------------------------------------
// Negative-interest disclosure (Task 19.3.21 §3)
// ---------------------------------------------------------------------------

// NegativeInterestDisclosure is the per-currency disclosure a client
// receives when negative rates apply — the allocation is disclosed and
// itemized, never silently netted into the balance.
type NegativeInterestDisclosure struct {
	Currency       string `json:"currency"`
	AppliedBalance string `json:"applied_balance"` // client cash the charge applied to
	AnnualRateBps  string `json:"annual_rate_bps"` // disclosed rate (bps charged per annum)
	PeriodDays     int    `json:"period_days"`
	DayBasis       int    `json:"day_basis"` // ACT/360 money-market convention
	AmountDebited  string `json:"amount_debited"`
}

// NegativeInterestDayBasis is the ACT/360 money-market day basis.
const NegativeInterestDayBasis = 360

// DiscloseNegativeInterest computes the itemized charge:
// balance × rateBps/10⁴ × days/360. A non-positive rate or balance
// produces a zero-amount disclosure (still disclosed — the statement
// line exists so clients see the rate was evaluated, not skipped).
func DiscloseNegativeInterest(ccy string, balance, annualRateBps decimal.Decimal,
	days int) NegativeInterestDisclosure {

	d := NegativeInterestDisclosure{
		Currency:       ccy,
		AppliedBalance: balance.String(),
		AnnualRateBps:  annualRateBps.String(),
		PeriodDays:     days,
		DayBasis:       NegativeInterestDayBasis,
		AmountDebited:  "0",
	}
	if balance.IsPositive() && annualRateBps.IsPositive() && days > 0 {
		amt := balance.Mul(annualRateBps).Div(decimal.NewFromInt(10_000)).
			Mul(decimal.NewFromInt(int64(days))).
			Div(decimal.NewFromInt(NegativeInterestDayBasis)).Round(8)
		d.AmountDebited = amt.String()
	}
	return d
}

// ---------------------------------------------------------------------------
// Quarterly re-validation report (Task 19.3.13 §5 — venue governance
// evidence for Task 21.3.15)
// ---------------------------------------------------------------------------

// RevalidationReport is the quarterly export: every run in the window,
// totals by kind/status, the breach ledger and the worst stress
// drawdown observed — serialized as evidence for venue governance.
type RevalidationReport struct {
	WindowStart   time.Time `json:"window_start"`
	WindowEnd     time.Time `json:"window_end"`
	StressRuns    int       `json:"stress_runs"`
	BacktestRuns  int       `json:"backtest_runs"`
	FailedRuns    int       `json:"failed_runs"`
	TotalBreaches int       `json:"total_breaches"`
	WorstDrawdown string    `json:"worst_drawdown_usd"`
	RunIDs        []int64   `json:"run_ids"`
	GeneratedAt   time.Time `json:"generated_at"`
}

// GenerateRevalidationReport aggregates the window's runs. Runs
// missing the window boundary are simply absent — the report reflects
// the register, never fabricates coverage.
func GenerateRevalidationReport(ctx context.Context, store ModelRunStore,
	start, end time.Time, now func() time.Time) (*RevalidationReport, error) {

	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	rep := &RevalidationReport{
		WindowStart: start, WindowEnd: end, GeneratedAt: now().UTC(),
	}
	for _, kind := range []string{RunKindStress, RunKindBacktest} {
		runs, err := store.ListRuns(ctx, kind, start)
		if err != nil {
			return nil, err
		}
		for _, r := range runs {
			if r.CreatedAt.After(end) {
				continue
			}
			if kind == RunKindStress {
				rep.StressRuns++
			} else {
				rep.BacktestRuns++
			}
			if r.Status == RunStatusFail {
				rep.FailedRuns++
			}
			rep.TotalBreaches += r.BreachCount
			rep.RunIDs = append(rep.RunIDs, r.RunID)
			// The suite stamps worst-case drawdown into result_metrics
			// for STRESS rows — surface the max across the window.
			if kind == RunKindStress && len(r.ResultMetrics) > 0 {
				var m struct {
					FundDrawdownUSD string `json:"fund_drawdown_usd"`
				}
				if err := json.Unmarshal(r.ResultMetrics, &m); err == nil &&
					m.FundDrawdownUSD != "" {
					d, derr := decimal.NewFromString(m.FundDrawdownUSD)
					if derr == nil {
						cur, cerr := decimal.NewFromString(rep.WorstDrawdown)
						if cerr != nil || d.GreaterThan(cur) {
							rep.WorstDrawdown = d.String()
						}
					}
				}
			}
		}
	}
	if rep.WorstDrawdown == "" {
		rep.WorstDrawdown = "0"
	}
	return rep, nil
}
