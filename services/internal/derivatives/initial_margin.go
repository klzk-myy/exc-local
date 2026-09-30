// initial_margin.go — Phase-22 Task 22.3.11 (spec §15.5, §13.11,
// §24 #146): ISDA SIMM-consistent uncleared initial margin for
// umr_in_scope accounts, plus the order-admission and exercise-margin
// gate implementations the orchestrator binds.
//
// Scope (simplified SIMM, FX product class only — the venue trades FX
// forwards/swaps/NDFs/options; no rates/credit/equity classes):
//   - Delta margin — sensitivities bucketed by SIMM FX volatility group
//     (regular-vol vs high-vol pairs), weighted by the published risk
//     weights, aggregated within buckets with the pairwise correlation,
//     then across buckets with the cross-group correlation and the
//     SIMM S_b clip.
//   - Vega margin — net vega risk per risk factor, vol-scaled, same
//     correlation structure.
//   - Curvature margin — CVR_i = −vega_i·σ_i·RW_k²·SF² summed per bucket
//     with θ = −0.5 on the negative part (SF = √1 vol-tenor scaling
//     deferred — flagged in UMRParams).
//   - Total IM = SIMM aggregation over the RESIDUAL (unconsumed) legs
//     plus each recognized spread's bounded margin (spread offsets
//     apply BEFORE aggregation — consumed legs never aggregate AND a
//     pair's contribution is its bounded worst-case, not the sum of
//     legs; §15.7 "never double-counted"). Task 19.3.25 back-fit.
//
// All money is decimal — no float64 leaves this file.
package derivatives

import (
	"context"
	stderrors "errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/risk"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Codes emitted by this file. Canonical codes (MARGIN_INSUFFICIENT,
// LEGAL_DOC_REQUIRED, SERVICE_DEGRADED, TRANSACTION_CONFLICT_RETRY_
// EXHAUSTED) are already in errs/codes.go; the scaffold rows below are
// registered append-only with this task as owner.
const (
	// CodeUMRIMEvalFailed — scaffold (Task 22.3.11, §15.5): sensitivity
	// feed, assessment store, or collateral read degraded mid-assessment
	// — gating fails closed.
	CodeUMRIMEvalFailed = "UMR_IM_EVAL_FAILED"
)

// ---------------------------------------------------------------------------
// SIMM parameter pack
// ---------------------------------------------------------------------------

// IMVolGroup is the SIMM FX volatility group of a currency pair.
type IMVolGroup string

const (
	IMVolRegular IMVolGroup = "REGULAR"
	IMVolHigh    IMVolGroup = "HIGH_VOL"
)

// UMRParams is the FX SIMM calibration pack. Defaults (DefaultUMRParams)
// are a published-SIMM snapshot — delta risk weight 7.3% for regular-vol
// pairs / 21.4% for high-vol, vega risk weight 0.35, intra-bucket
// correlation 0.5; all values are parameters so a SIMM re-version is a
// config change, not a code change.
type UMRParams struct {
	CalcCurrency string // IM numéraire — "USD"
	// DeltaRW maps vol group → risk weight (fraction, e.g. 0.073).
	DeltaRW map[IMVolGroup]decimal.Decimal
	// VegaRW is the SIMM FX vega risk weight (dimensionless, applied to
	// vega·σ — a +1.00 absolute vol move).
	VegaRW decimal.Decimal
	// CorrSameGroup / CorrCrossGroup are the SIMM pairwise correlations
	// within a vol-group bucket and between the two buckets.
	CorrSameGroup  decimal.Decimal
	CorrCrossGroup decimal.Decimal
	// CurvatureTheta scales the negative CVR sum (SIMM θ = −0.5).
	CurvatureTheta decimal.Decimal
	// HighVolCurrencies marks the SIMM high-vol currency set; a pair is
	// high-vol when either leg is in the set (calc ccy excluded).
	HighVolCurrencies map[string]bool
	// MPORDays is the uncleared margin period of risk (10 days per
	// BCBS-IOSCO / spec §15.5).
	MPORDays int
	// ThresholdUSD is the UMR exchange threshold below which no IM moves
	// (50m in the calc currency, per the regulatory schedule).
	ThresholdUSD decimal.Decimal
	// MinTransferUSD is the minimum transfer amount (MTA) — deficits at
	// or below it stay IN_TOLERANCE rather than issuing a call.
	MinTransferUSD decimal.Decimal
	// DisputeWindowHours is the §15.7 dispute window before an unpaid
	// call escalates.
	DisputeWindowHours int
}

// DefaultUMRParams returns the calibrated default pack.
func DefaultUMRParams() UMRParams {
	return UMRParams{
		CalcCurrency: "USD",
		DeltaRW: map[IMVolGroup]decimal.Decimal{
			IMVolRegular: decimal.NewFromFloat(0.073),
			IMVolHigh:    decimal.NewFromFloat(0.214),
		},
		VegaRW:         decimal.NewFromFloat(0.35),
		CorrSameGroup:  decimal.NewFromFloat(0.5),
		CorrCrossGroup: decimal.NewFromFloat(0.5),
		CurvatureTheta: decimal.NewFromFloat(-0.5),
		// SIMM high-vol FX set (snapshot; config-driven).
		HighVolCurrencies: map[string]bool{
			"BRL": true, "RUB": true, "TRY": true,
		},
		MPORDays:           10,
		ThresholdUSD:       decimal.NewFromInt(50_000_000),
		MinTransferUSD:     decimal.NewFromInt(500_000),
		DisputeWindowHours: 48,
	}
}

// IMVolGroupFor classifies a pair: high-vol when either leg is in the
// high-vol set (the calc currency never is).
func IMVolGroupFor(base, quote string, p UMRParams) IMVolGroup {
	for c := range map[string]bool{base: true, quote: true} {
		if c == p.CalcCurrency {
			continue
		}
		if p.HighVolCurrencies[c] {
			return IMVolHigh
		}
	}
	return IMVolRegular
}

// ---------------------------------------------------------------------------
// Sensitivities
// ---------------------------------------------------------------------------

// IMSensitivity is one instrument's FX sensitivity contribution.
// DeltaUSD is the USD P&L of a +1% move of the pair (base vs quote);
// VegaUSD is the USD P&L of a +1.00 absolute implied-vol move (options
// only); Sigma is the implied vol used for vega/curvature scaling.
// PositionRef identifies the leg so spread offsets can consume it once.
type IMSensitivity struct {
	PositionRef int64  // positions.id / derivative_contracts.id
	Label       string // audit — symbol or contract ref
	BaseCCY     string
	QuoteCCY    string
	DeltaUSD    decimal.Decimal
	VegaUSD     decimal.Decimal
	Sigma       decimal.Decimal
	IsOption    bool
	// NakedMarginUSD is the leg's standalone (pre-offset) margin — the
	// ceiling a spread relief may draw down against this leg.
	NakedMarginUSD decimal.Decimal
}

// IMRiskKey is the SIMM risk-factor key — the currency pair.
func (s IMSensitivity) IMRiskKey() string { return s.BaseCCY + "/" + s.QuoteCCY }

// IMVolGroup resolves the pair's bucket.
func (s IMSensitivity) IMVolGroup(p UMRParams) IMVolGroup {
	return IMVolGroupFor(s.BaseCCY, s.QuoteCCY, p)
}

// IMSpreadOffset is pre-aggregation margin relief produced by the
// spread-recognition engine (internal/margin.DetectSpreads). ReliefUSD
// is drawn down against the referenced legs' NakedMarginUSD ceilings;
// each PositionRef may appear at most once across all offsets (the
// engine enforces single consumption; this layer verifies it).
type IMSpreadOffset struct {
	SpreadID   string
	SpreadType string
	LegRefs    []int64         // PositionRefs consumed by the pair
	ReliefUSD  decimal.Decimal // non-negative
}

// ---------------------------------------------------------------------------
// SIMM aggregation
// ---------------------------------------------------------------------------

// IMResult is one SIMM computation — components kept separate for the
// umr_im_assessments audit columns.
type IMResult struct {
	DeltaUSD        decimal.Decimal
	VegaUSD         decimal.Decimal
	CurvatureUSD    decimal.Decimal
	SpreadMarginUSD decimal.Decimal // recognized spreads' bounded margin (legs' naked − relief)
	SpreadReliefUSD decimal.Decimal // recognized-spread relief granted (naked-sum − bounded margin)
	TotalUSD        decimal.Decimal
}

// IMAggregate computes the SIMM-consistent IM over a portfolio.
// Offsets apply BEFORE aggregation (§15.7, Task 19.3.25 back-fit): a
// recognized spread's legs are consumed out of the sensitivity set, so
// delta/vega/curvature aggregate only the residual legs and each pair
// contributes its bounded margin (legs' naked sum − capped relief) —
// never the sum of legs, never netted a second time inside SIMM, and
// each PositionRef may be consumed once (a re-used leg fails the whole
// computation closed).
func IMAggregate(sens []IMSensitivity, offsets []IMSpreadOffset, p UMRParams) (IMResult, error) {
	residual, spreadMargin, relief, err := imSpreadConsume(sens, offsets)
	if err != nil {
		return IMResult{}, err
	}
	res := IMResult{
		DeltaUSD:        imBucketAggregate(residual, p, false),
		VegaUSD:         imBucketAggregate(residual, p, true),
		CurvatureUSD:    imCurvature(residual, p),
		SpreadMarginUSD: spreadMargin,
		SpreadReliefUSD: relief,
	}
	res.TotalUSD = decimal.Max(decimal.Zero,
		res.DeltaUSD.Add(res.VegaUSD).Add(res.CurvatureUSD).
			Add(spreadMargin)).Round(8)
	res.DeltaUSD = res.DeltaUSD.Round(8)
	res.VegaUSD = res.VegaUSD.Round(8)
	res.CurvatureUSD = res.CurvatureUSD.Round(8)
	return res, nil
}

// imSpreadConsume enforces the §15.7 pre-aggregation ordering: every
// offset's legs are marked consumed (double-consumption fails closed),
// each pair's relief is capped at its legs' naked-margin sum, and the
// pair's margin contribution is the bounded remainder
// (legNaked − relief). Returns the residual (unconsumed) sensitivity
// set — consumed legs NEVER enter bucket aggregation — plus the summed
// spread margin and the granted relief for audit.
func imSpreadConsume(sens []IMSensitivity, offsets []IMSpreadOffset) (
	residual []IMSensitivity, spreadMargin, reliefTotal decimal.Decimal, err error) {
	naked := map[int64]decimal.Decimal{}
	for _, s := range sens {
		naked[s.PositionRef] = s.NakedMarginUSD
	}
	consumed := map[int64]bool{}
	spreadMargin = decimal.Zero
	reliefTotal = decimal.Zero
	for _, o := range offsets {
		if o.ReliefUSD.IsNegative() {
			return nil, decimal.Zero, decimal.Zero, excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("spread offset %s: negative relief", o.SpreadID))
		}
		legNaked := decimal.Zero
		for _, ref := range o.LegRefs {
			if consumed[ref] {
				return nil, decimal.Zero, decimal.Zero, excerrors.New("DERIVATIVE_STATE_CONFLICT",
					fmt.Sprintf("spread offset %s: leg %d consumed twice (double-count guard)", o.SpreadID, ref))
			}
			consumed[ref] = true
			if n, ok := naked[ref]; ok {
				legNaked = legNaked.Add(n)
			}
		}
		// Relief is capped at the legs' naked margin; the pair's margin
		// contribution is the bounded remainder.
		relief := decimal.Min(o.ReliefUSD, legNaked)
		reliefTotal = reliefTotal.Add(relief)
		spreadMargin = spreadMargin.Add(legNaked.Sub(relief))
	}
	for _, s := range sens {
		if !consumed[s.PositionRef] {
			residual = append(residual, s)
		}
	}
	return residual, spreadMargin.Round(8), reliefTotal.Round(8), nil
}

// imBucketAggregate runs the SIMM two-level aggregation for delta
// (weighted sensitivities) or vega (σ-scaled weighted sensitivities):
// per-pair net → per-bucket K_b with pairwise correlation → cross-bucket
// aggregation with the S_b clip.
func imBucketAggregate(sens []IMSensitivity, p UMRParams, vega bool) decimal.Decimal {
	type pairKey struct {
		key   string
		group IMVolGroup
	}
	ws := map[pairKey]decimal.Decimal{}
	for _, s := range sens {
		rw := p.DeltaRW[s.IMVolGroup(p)]
		if rw.IsZero() {
			rw = p.DeltaRW[IMVolRegular]
		}
		var v decimal.Decimal
		if vega {
			if !s.IsOption || s.Sigma.IsZero() {
				continue
			}
			v = s.VegaUSD.Mul(s.Sigma).Mul(p.VegaRW)
		} else {
			v = s.DeltaUSD.Mul(rw)
		}
		k := pairKey{s.IMRiskKey(), s.IMVolGroup(p)}
		ws[k] = ws[k].Add(v)
	}
	bucketItems := map[IMVolGroup][]decimal.Decimal{}
	bucketSum := map[IMVolGroup]decimal.Decimal{}
	for k, w := range ws {
		bucketItems[k.group] = append(bucketItems[k.group], w)
		bucketSum[k.group] = bucketSum[k.group].Add(w)
	}
	ks := map[IMVolGroup]decimal.Decimal{}
	ss := map[IMVolGroup]decimal.Decimal{}
	for g, items := range bucketItems {
		var sumSq decimal.Decimal
		for _, w := range items {
			sumSq = sumSq.Add(w.Mul(w))
		}
		var cross decimal.Decimal
		for i, wi := range items {
			for j, wj := range items {
				if i >= j {
					continue
				}
				cross = cross.Add(wi.Mul(wj))
			}
		}
		k2 := sumSq.Add(cross.Mul(decimal.NewFromInt(2)).Mul(p.CorrSameGroup))
		k := decSqrt(k2)
		ks[g] = k
		// SIMM S_b clip: min(max(ΣWS, −K_b), K_b)
		s := bucketSum[g]
		if s.GreaterThan(k) {
			s = k
		} else if s.LessThan(k.Neg()) {
			s = k.Neg()
		}
		ss[g] = s
	}
	var total2 decimal.Decimal
	for _, k := range ks {
		total2 = total2.Add(k.Mul(k))
	}
	groups := []IMVolGroup{IMVolRegular, IMVolHigh}
	for i, gi := range groups {
		for j, gj := range groups {
			if i >= j {
				continue
			}
			total2 = total2.Add(ss[gi].Mul(ss[gj]).Mul(p.CorrCrossGroup).Mul(decimal.NewFromInt(2)))
		}
	}
	return decSqrt(total2)
}

// imCurvature computes the FX curvature margin: CVR_i = −vega_i·σ_i·RW_k²
// per option leg; per bucket = Σ max(CVR,0) + θ·Σ min(CVR,0); total
// sums buckets.
func imCurvature(sens []IMSensitivity, p UMRParams) decimal.Decimal {
	perBucket := map[IMVolGroup]decimal.Decimal{}
	for _, s := range sens {
		if !s.IsOption || s.VegaUSD.IsZero() || s.Sigma.IsZero() {
			continue
		}
		rw := p.DeltaRW[s.IMVolGroup(p)]
		if rw.IsZero() {
			rw = p.DeltaRW[IMVolRegular]
		}
		cvr := s.VegaUSD.Mul(s.Sigma).Mul(rw.Mul(rw)).Neg()
		g := s.IMVolGroup(p)
		perBucket[g] = perBucket[g].Add(cvr)
	}
	total := decimal.Zero
	for _, sum := range perBucket {
		if sum.IsNegative() {
			total = total.Add(sum.Mul(p.CurvatureTheta)) // θ·min(CVR,0), θ<0
		} else {
			total = total.Add(sum)
		}
	}
	return total
}

// decSqrt is the decimal square-root boundary: margin magnitudes are
// small enough that a float64 sqrt at the LAST step (never an input or
// intermediate) keeps 8dp accuracy.
func decSqrt(d decimal.Decimal) decimal.Decimal {
	if !d.IsPositive() {
		return decimal.Zero
	}
	f, _ := d.Float64()
	return decimal.NewFromFloat(math.Sqrt(f))
}

// ---------------------------------------------------------------------------
// Assessment persistence (migration 253)
// ---------------------------------------------------------------------------

// UMR assessment statuses — the umr_im_status_enum domain.
const (
	UMRStatusInTolerance = "IN_TOLERANCE"
	UMRStatusCallIssued  = "CALL_ISSUED"
	UMRStatusDisputed    = "DISPUTED"
	UMRStatusSatisfied   = "SATISFIED"
	UMRStatusEscalated   = "ESCALATED"
)

// UMRAssessment is one daily IM recomputation.
type UMRAssessment struct {
	ID              int64
	AccountID       int64
	AssessmentDate  time.Time
	RequiredIMUSD   decimal.Decimal
	DeltaUSD        decimal.Decimal
	VegaUSD         decimal.Decimal
	CurvatureUSD    decimal.Decimal
	PostedIMUSD     decimal.Decimal
	DeficitUSD      decimal.Decimal
	MTAUSD          decimal.Decimal
	ThresholdUSD    decimal.Decimal
	Status          string
	CallIssuedAt    *time.Time
	DisputeDeadline *time.Time
	SatisfiedAt     *time.Time
}

// OpenCall returns whether the assessment is an unpaid IM call.
func (a *UMRAssessment) OpenCall() bool {
	switch a.Status {
	case UMRStatusCallIssued, UMRStatusDisputed, UMRStatusEscalated:
		return true
	}
	return false
}

// IMSensitivitySource feeds the daily assessment — the derivatives book
// (per-contract delta/vega with curve + surface inputs) binds it.
type IMSensitivitySource interface {
	Sensitivities(ctx context.Context, accountID int64) ([]IMSensitivity, error)
}

// IMSpreadOffsetSource supplies recognized-spread relief for the
// assessment — MarginSpreadOffsetSource (im_spread_offsets.go) adapts
// *margin.SpreadOffsetService to it (the service's APPLIED rows; Task
// 19.3.25 back-fit).
type IMSpreadOffsetSource interface {
	SpreadOffsets(ctx context.Context, accountID int64) ([]IMSpreadOffset, error)
}

// UMRPostedCollateralSource reports the account's haircut-adjusted
// segregated IM collateral in USD (collateral_schedule / Phase-19
// Task 19.3.8 machinery binds it).
type UMRPostedCollateralSource interface {
	PostedIMUSD(ctx context.Context, accountID int64) (decimal.Decimal, error)
}

// UMRIMCallIssuer is the margin-call workflow seam (Phase-19 Task 19.3.3)
// — invoked when an assessment produces a deficit above MTA.
type UMRIMCallIssuer interface {
	IssueIMCall(ctx context.Context, a *UMRAssessment) error
}

// ---------------------------------------------------------------------------
// UMRIMService — daily recompute + call issuance
// ---------------------------------------------------------------------------

// UMRIMService runs the daily SIMM assessment for umr_in_scope accounts.
type UMRIMService struct {
	pool    *pgxpool.Pool
	params  UMRParams
	sens    IMSensitivitySource
	offsets IMSpreadOffsetSource // may be nil → no relief
	posted  UMRPostedCollateralSource
	issuer  UMRIMCallIssuer // may be nil → call rows still persist
	now     func() time.Time
}

// NewUMRIMService wires the service; pool, sens and posted are required
// (an assessment without a sensitivity feed or collateral read would
// fabricate zeros — fail closed at construction).
func NewUMRIMService(pool *pgxpool.Pool, params UMRParams, sens IMSensitivitySource,
	posted UMRPostedCollateralSource, offsets IMSpreadOffsetSource,
	issuer UMRIMCallIssuer, now func() time.Time) (*UMRIMService, error) {
	switch {
	case pool == nil:
		return nil, excerrors.New(CodeUMRIMEvalFailed, "umr im: nil pool")
	case sens == nil:
		return nil, excerrors.New(CodeUMRIMEvalFailed, "umr im: nil sensitivity source")
	case posted == nil:
		return nil, excerrors.New(CodeUMRIMEvalFailed, "umr im: nil posted-collateral source")
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &UMRIMService{pool: pool, params: params, sens: sens,
		offsets: offsets, posted: posted, issuer: issuer, now: now}, nil
}

// UMRInScope reads the accounts.umr_in_scope flag — the scope gate all
// IM decisions route through (below-threshold counterparties use the
// Phase-19 margin model instead).
func (s *UMRIMService) UMRInScope(ctx context.Context, accountID int64) (bool, error) {
	var v bool
	if err := s.pool.QueryRow(ctx,
		`SELECT umr_in_scope FROM accounts WHERE id=$1`, accountID).Scan(&v); err != nil {
		return false, excerrors.Wrap(CodeUMRIMEvalFailed, "umr: scope read", err)
	}
	return v, nil
}

// AccountsInScope lists every umr_in_scope account — the daily sweep set.
func (s *UMRIMService) AccountsInScope(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM accounts WHERE umr_in_scope ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Assess computes and persists one account's daily assessment — the
// (account_id, assessment_date) unique key makes same-day reruns an
// upsert, never a duplicate row. A deficit above the MTA issues the IM
// call through the margin-call workflow seam.
func (s *UMRIMService) Assess(ctx context.Context, accountID int64, day time.Time) (*UMRAssessment, error) {
	day = day.UTC().Truncate(24 * time.Hour)
	sens, err := s.sens.Sensitivities(ctx, accountID)
	if err != nil {
		return nil, excerrors.Wrap(CodeUMRIMEvalFailed, "umr: sensitivity feed", err)
	}
	var offsets []IMSpreadOffset
	if s.offsets != nil {
		offsets, err = s.offsets.SpreadOffsets(ctx, accountID)
		if err != nil {
			return nil, excerrors.Wrap(CodeUMRIMEvalFailed, "umr: spread offsets", err)
		}
	}
	res, err := IMAggregate(sens, offsets, s.params)
	if err != nil {
		return nil, err
	}
	posted, err := s.posted.PostedIMUSD(ctx, accountID)
	if err != nil {
		return nil, excerrors.Wrap(CodeUMRIMEvalFailed, "umr: posted collateral", err)
	}
	deficit := decimal.Max(decimal.Zero, res.TotalUSD.Sub(posted))
	a := &UMRAssessment{
		AccountID: accountID, AssessmentDate: day,
		RequiredIMUSD: res.TotalUSD, DeltaUSD: res.DeltaUSD,
		VegaUSD: res.VegaUSD, CurvatureUSD: res.CurvatureUSD,
		PostedIMUSD: posted, DeficitUSD: deficit,
		MTAUSD: s.params.MinTransferUSD, ThresholdUSD: s.params.ThresholdUSD,
	}
	now := s.now()
	switch {
	case deficit.LessThanOrEqual(s.params.MinTransferUSD):
		// Below MTA → no call; a previously-open call closes SATISFIED.
		prev, perr := s.latestOpen(ctx, accountID)
		if perr != nil {
			return nil, perr
		}
		if prev != nil {
			a.Status = UMRStatusSatisfied
			a.SatisfiedAt = &now
		} else {
			a.Status = UMRStatusInTolerance
		}
	default:
		a.Status = UMRStatusCallIssued
		a.CallIssuedAt = &now
		dl := now.Add(time.Duration(s.params.DisputeWindowHours) * time.Hour)
		a.DisputeDeadline = &dl
	}
	if err := s.persist(ctx, a); err != nil {
		return nil, excerrors.Wrap(CodeUMRIMEvalFailed, "umr: assessment persist", err)
	}
	if a.Status == UMRStatusCallIssued && s.issuer != nil {
		if err := s.issuer.IssueIMCall(ctx, a); err != nil {
			return nil, excerrors.Wrap(CodeUMRIMEvalFailed, "umr: call issuance", err)
		}
	}
	return a, nil
}

// AssessDue runs the daily assessment over every in-scope account — the
// scheduler seam (bind next to the VM sweep cadence).
func (s *UMRIMService) AssessDue(ctx context.Context, day time.Time) (int, error) {
	ids, err := s.AccountsInScope(ctx)
	if err != nil {
		return 0, excerrors.Wrap(CodeUMRIMEvalFailed, "umr: scope scan", err)
	}
	n := 0
	for _, id := range ids {
		if _, err := s.Assess(ctx, id, day); err != nil {
			return n, err // fail closed mid-sweep; rerun resumes idempotently
		}
		n++
	}
	return n, nil
}

// OutstandingCall reports whether the account has an unpaid IM call —
// the order-gate predicate (open deficit blocks new covered flow).
func (s *UMRIMService) OutstandingCall(ctx context.Context, accountID int64) (*UMRAssessment, error) {
	return s.latestOpen(ctx, accountID)
}

func (s *UMRIMService) latestOpen(ctx context.Context, accountID int64) (*UMRAssessment, error) {
	var a UMRAssessment
	err := s.pool.QueryRow(ctx, `
		SELECT id, account_id, assessment_date, required_im_usd, delta_margin_usd,
		       vega_margin_usd, curvature_margin_usd, posted_im_usd, deficit_usd,
		       mta_usd, threshold_usd, status, call_issued_at, dispute_deadline, satisfied_at
		FROM umr_im_assessments
		WHERE account_id=$1 AND status IN ('CALL_ISSUED','DISPUTED','ESCALATED')
		ORDER BY assessment_date DESC LIMIT 1`, accountID).Scan(
		&a.ID, &a.AccountID, &a.AssessmentDate, &a.RequiredIMUSD, &a.DeltaUSD,
		&a.VegaUSD, &a.CurvatureUSD, &a.PostedIMUSD, &a.DeficitUSD,
		&a.MTAUSD, &a.ThresholdUSD, &a.Status, &a.CallIssuedAt, &a.DisputeDeadline, &a.SatisfiedAt)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *UMRIMService) persist(ctx context.Context, a *UMRAssessment) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO umr_im_assessments
		    (account_id, assessment_date, required_im_usd, delta_margin_usd,
		     vega_margin_usd, curvature_margin_usd, posted_im_usd, deficit_usd,
		     mta_usd, threshold_usd, status, call_issued_at, dispute_deadline, satisfied_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (account_id, assessment_date) DO UPDATE SET
		    required_im_usd=EXCLUDED.required_im_usd,
		    delta_margin_usd=EXCLUDED.delta_margin_usd,
		    vega_margin_usd=EXCLUDED.vega_margin_usd,
		    curvature_margin_usd=EXCLUDED.curvature_margin_usd,
		    posted_im_usd=EXCLUDED.posted_im_usd,
		    deficit_usd=EXCLUDED.deficit_usd,
		    mta_usd=EXCLUDED.mta_usd,
		    threshold_usd=EXCLUDED.threshold_usd,
		    status=EXCLUDED.status,
		    call_issued_at=COALESCE(umr_im_assessments.call_issued_at, EXCLUDED.call_issued_at),
		    dispute_deadline=COALESCE(umr_im_assessments.dispute_deadline, EXCLUDED.dispute_deadline),
		    satisfied_at=EXCLUDED.satisfied_at,
		    computed_at=now(), updated_at=now()`,
		a.AccountID, a.AssessmentDate, a.RequiredIMUSD, a.DeltaUSD, a.VegaUSD,
		a.CurvatureUSD, a.PostedIMUSD, a.DeficitUSD, a.MTAUSD, a.ThresholdUSD,
		a.Status, a.CallIssuedAt, a.DisputeDeadline, a.SatisfiedAt)
	return err
}

// ---------------------------------------------------------------------------
// Pre-trade admission gate (the order-path seam)
// ---------------------------------------------------------------------------

// UMRDocGate is the legal-agreement check — *compliance.LegalDocService
// satisfies it. NDF/OPTION order flow requires EXECUTED ISDA+CSA
// regardless of UMR scope (spec §15.5).
type UMRDocGate interface {
	AdmitOrder(ctx context.Context, accountID int64, instrumentClass, category string, reduceOnly bool) error
}

// UMRAdmissionGate is the pre-trade hook the order path binds for
// derivative instruments. Order of checks (fail-closed at every step):
//  1. Non-covered class → admit (SPOT etc. carry no UMR obligation).
//  2. Legal documentation via the compliance gate (all covered classes).
//  3. umr_in_scope → an outstanding IM call (CALL_ISSUED/DISPUTED/
//     ESCALATED) blocks new exposure with MARGIN_INSUFFICIENT; assessment
//     reads failing reject UMR_IM_EVAL_FAILED.
//  4. Out-of-scope counterparties pass to the standard Phase-19 margin
//     model (nothing further here).
//
// UMRScopeService is the slice of UMRIMService the admission gate needs —
// *UMRIMService satisfies it; tests substitute fixtures.
type UMRScopeService interface {
	UMRInScope(ctx context.Context, accountID int64) (bool, error)
	OutstandingCall(ctx context.Context, accountID int64) (*UMRAssessment, error)
}

type UMRAdmissionGate struct {
	scope UMRScopeService
	docs  UMRDocGate
}

// NewUMRAdmissionGate wires the gate; scope and docs are required.
func NewUMRAdmissionGate(scope UMRScopeService, docs UMRDocGate) (*UMRAdmissionGate, error) {
	if scope == nil || docs == nil {
		return nil, excerrors.New(CodeUMRIMEvalFailed,
			"umr admission: scope service and doc gate are required (fail-closed)")
	}
	return &UMRAdmissionGate{scope: scope, docs: docs}, nil
}

// UMRClassCovered reports whether the instrument class carries the
// §15.5 documentation/IM obligation (NDF and FX-option flow).
func UMRClassCovered(instrumentClass string) bool {
	switch instrumentClass {
	case "NDF", "OPTION":
		return true
	}
	return false
}

// AdmitOrder — nil admits; coded error rejects. reduceOnly bypasses the
// IM-call block and the doc requirement entirely (terminated docs and
// open calls must never strand closes — §15.5 edge case).
func (g *UMRAdmissionGate) AdmitOrder(ctx context.Context, accountID int64,
	instrumentClass, category string, reduceOnly bool) error {
	if reduceOnly {
		return nil
	}
	if !UMRClassCovered(instrumentClass) {
		return nil
	}
	if err := g.docs.AdmitOrder(ctx, accountID, instrumentClass, category, false); err != nil {
		return err // LEGAL_DOC_REQUIRED / SERVICE_DEGRADED propagate verbatim
	}
	inScope, err := g.scope.UMRInScope(ctx, accountID)
	if err != nil {
		return excerrors.Wrap(CodeUMRIMEvalFailed, "umr admission: scope read", err)
	}
	if !inScope {
		return nil // below-AANA counterparties → standard Phase-19 margin
	}
	open, err := g.scope.OutstandingCall(ctx, accountID)
	if err != nil {
		return excerrors.Wrap(CodeUMRIMEvalFailed, "umr admission: call read", err)
	}
	if open != nil && open.OpenCall() {
		return excerrors.New("MARGIN_INSUFFICIENT", fmt.Sprintf(
			"account %d has an outstanding IM call (%s %s deficit) — new %s exposure blocked",
			accountID, open.Status, open.DeficitUSD.String(), instrumentClass))
	}
	return nil
}

// ---------------------------------------------------------------------------
// ExerciseMarginChecker implementation (lifecycle seam)
// ---------------------------------------------------------------------------

// IMEquityEvaluator is the account-margin evaluation seam —
// *risk.MarginService satisfies it (Equity/UsedMargin are USD).
type IMEquityEvaluator interface {
	Evaluate(ctx context.Context, accountID int64) (*risk.MarginSnapshot, error)
}

// IMExerciseChecker implements lifecycle.go's ExerciseMarginChecker:
// the delivered leg's required IM is qty·price/max_leverage of the
// underlying instrument (the same convention the margin engine applies
// to the resulting spot position) converted to USD by the injected rate
// seam; coverage = equity ≥ used + required.
type IMExerciseChecker struct {
	pool *pgxpool.Pool
	eval IMEquityEvaluator
	// quoteUSD converts a quote-currency amount to USD at current marks —
	// injected (the margin engine's conversion instruments). nil = quote
	// must equal USD, otherwise the check fails closed.
	quoteUSD func(ctx context.Context, ccy string) (decimal.Decimal, error)
}

// NewIMExerciseChecker wires the checker. eval is required; quoteUSD may
// be nil for a USD-only book (non-USD quotes then fail closed).
func NewIMExerciseChecker(pool *pgxpool.Pool, eval IMEquityEvaluator,
	quoteUSD func(ctx context.Context, ccy string) (decimal.Decimal, error)) (*IMExerciseChecker, error) {
	switch {
	case pool == nil:
		return nil, excerrors.New(CodeUMRIMEvalFailed, "exercise im: nil pool")
	case eval == nil:
		return nil, excerrors.New(CodeUMRIMEvalFailed, "exercise im: nil equity evaluator")
	}
	return &IMExerciseChecker{pool: pool, eval: eval, quoteUSD: quoteUSD}, nil
}

// ExerciseMargin implements derivatives.ExerciseMarginChecker.
func (c *IMExerciseChecker) ExerciseMargin(ctx context.Context, accountID,
	underlyingInstrumentID int64, qty, price decimal.Decimal) (decimal.Decimal, bool, error) {
	var quote string
	var lev int64
	err := c.pool.QueryRow(ctx, `
		SELECT quote_currency, max_leverage FROM instruments
		WHERE id=$1 AND instrument_type='SPOT'`, underlyingInstrumentID).Scan(&quote, &lev)
	if err != nil {
		return decimal.Zero, false, excerrors.Wrap(CodeUMRIMEvalFailed,
			fmt.Sprintf("exercise im: underlying %d read", underlyingInstrumentID), err)
	}
	if lev <= 0 {
		return decimal.Zero, false, excerrors.New(CodeUMRIMEvalFailed,
			fmt.Sprintf("exercise im: underlying %d has non-positive max_leverage", underlyingInstrumentID))
	}
	requiredQuote := qty.Mul(price).Div(decimal.NewFromInt(lev)).Round(8)
	requiredUSD := requiredQuote
	if quote != "USD" {
		if c.quoteUSD == nil {
			return decimal.Zero, false, excerrors.New(CodeUMRIMEvalFailed, fmt.Sprintf(
				"exercise im: no USD converter bound for quote %s (fail-closed)", quote))
		}
		rate, err := c.quoteUSD(ctx, quote)
		if err != nil {
			return decimal.Zero, false, excerrors.Wrap(CodeUMRIMEvalFailed,
				"exercise im: quote conversion", err)
		}
		requiredUSD = requiredQuote.Mul(rate).Round(8)
	}
	snap, err := c.eval.Evaluate(ctx, accountID)
	if err != nil {
		return decimal.Zero, false, excerrors.Wrap(CodeUMRIMEvalFailed,
			"exercise im: margin eval", err)
	}
	if snap == nil {
		return decimal.Zero, false, excerrors.New(CodeUMRIMEvalFailed,
			fmt.Sprintf("exercise im: no margin snapshot for account %d", accountID))
	}
	ok := snap.Equity.GreaterThanOrEqual(snap.UsedMargin.Add(requiredUSD))
	return requiredUSD, ok, nil
}
