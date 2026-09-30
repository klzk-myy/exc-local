// treasury.go — Phase-24 Task 24.3.17: venue treasury, own funds and the
// stressed-liquidity buffer (spec §17.13.1, §24 #329).
//
// §17.9/Task 24.3.11 protects client money; this file protects the entity
// behind it:
//
//   - own_funds_balances (mig 082) is the house balance sheet — house
//     equity, capital reserves, retained earnings and the insurance-fund
//     balance as first-class lines DISTINCT from client money and from
//     the GL, reconciled daily against bank statements via the
//     StatementSource seam.
//   - EvaluateLiquidity compares liquid house funds against a stressed
//     5-business-day outflow (forced withdrawals, adverse rollover,
//     insurance-fund call) at the configured minimum ratio; a breach
//     records the assessment, raises TREASURY_LIQUIDITY_BREACH (P1,
//     HTTP 503) and freezes discretionary outflows + new LP capacity via
//     treasury_controls until remediated under dual control.
//   - CheckInsuranceExpiries pages INSURANCE_POLICY_EXPIRING (P2, Finance
//     Ops + Compliance Officer) for cover expiring inside 60 days.
//   - AdmissionSatisfied is the Phase-21 Task 21.3.13 "minimum financial
//     resources" launch-prerequisite seam: funded own funds AND an
//     executed contingent-capital backstop — never attestation.
package backoffice

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/admin"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Own-funds line kinds (own_funds_balances.line_kind).
const (
	FundHouseEquity       = "HOUSE_EQUITY"
	FundCapitalReserve    = "CAPITAL_RESERVE"
	FundInsuranceFundLine = "INSURANCE_FUND"
	FundRetainedEarnings  = "RETAINED_EARNINGS"
)

// ownFundsKinds is the full ledger line set; liquidKinds excludes the
// insurance-fund line (earmarked client-protection capital is NOT
// discretionary liquidity).
var ownFundsKinds = []string{FundHouseEquity, FundCapitalReserve, FundInsuranceFundLine, FundRetainedEarnings}
var liquidKinds = []string{FundHouseEquity, FundCapitalReserve, FundRetainedEarnings}

// Own-funds reconciliation statuses.
const (
	ReconPending    = "PENDING"
	ReconReconciled = "RECONCILED"
	ReconBreak      = "BREAK"
)

// OwnFunds is one own_funds_balances row — a first-class house-capital
// line, reconciled daily against bank statements (§17.13.1.1).
type OwnFunds struct {
	ID                   int64            `json:"id"`
	LineKind             string           `json:"line_kind"`
	Currency             string           `json:"currency"`
	Balance              decimal.Decimal  `json:"balance"`
	ReconciliationStatus string           `json:"reconciliation_status"`
	StatementRef         string           `json:"statement_ref,omitempty"`
	StatementBalance     *decimal.Decimal `json:"statement_balance,omitempty"`
	ReconciledAt         *time.Time       `json:"reconciled_at,omitempty"`
	UpdatedAt            time.Time        `json:"updated_at"`
	CreatedAt            time.Time        `json:"created_at"`
}

// LiquidityAssessment is one treasury_liquidity_assessments row — the
// durable record of each stressed-buffer evaluation.
type LiquidityAssessment struct {
	ID                int64           `json:"id"`
	AssessedAt        time.Time       `json:"assessed_at"`
	Currency          string          `json:"currency"`
	LiquidHouseFunds  decimal.Decimal `json:"liquid_house_funds"`
	StressedOutflow5d decimal.Decimal `json:"stressed_outflow_5d"`
	RequiredBuffer    decimal.Decimal `json:"required_buffer"`
	Status            string          `json:"status"` // OK|BREACH
	Detail            json.RawMessage `json:"detail,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
}

// TreasuryControls is the single treasury_controls flag row — a liquidity
// breach freezes discretionary house outflows and blocks new LP capacity.
type TreasuryControls struct {
	DiscretionaryOutflowsFrozen bool       `json:"discretionary_outflows_frozen"`
	LPCapacityBlocked           bool       `json:"lp_capacity_blocked"`
	FrozenAt                    *time.Time `json:"frozen_at,omitempty"`
	FreezeReason                string     `json:"freeze_reason,omitempty"`
	UnfrozenBy                  *int64     `json:"unfrozen_by,omitempty"`
	UnfrozenAt                  *time.Time `json:"unfrozen_at,omitempty"`
}

// StressedOutflowSource supplies the stressed 5-business-day outflow per
// currency (forced withdrawals + adverse rollover + insurance-fund call)
// — production binds the Phase-19 stress/backtest suite or a treasury
// model; nil fails closed (EvaluateLiquidity refuses, never guesses).
type StressedOutflowSource interface {
	StressedOutflow5d(ctx context.Context, ccy string) (decimal.Decimal, error)
}

// TreasuryDeps wires TreasuryService.
type TreasuryDeps struct {
	Store    Store
	Outflows StressedOutflowSource
	Alerter  OpsAlerter
	Resolver RoleResolver
	Now      func() time.Time
	// LiquidityRatio is the minimum standing house-liquidity ratio vs the
	// stressed 5-day outflow (default 1.0 — liquid funds must fully cover).
	LiquidityRatio decimal.Decimal
}

// TreasuryService is the Task 24.3.17 engine. It owns own-funds
// reconciliation, the stressed-liquidity buffer and the admission gate;
// contingent-capital commitment mechanics live in contingent_capital.go.
type TreasuryService struct {
	store    Store
	outflows StressedOutflowSource
	alerter  OpsAlerter
	resolve  RoleResolver
	now      func() time.Time
	ratio    decimal.Decimal
}

// NewTreasuryService wires the service; Store and Resolver are mandatory.
func NewTreasuryService(d TreasuryDeps) (*TreasuryService, error) {
	if d.Store == nil {
		return nil, fmt.Errorf("treasury: nil store")
	}
	if d.Resolver == nil {
		return nil, fmt.Errorf("treasury: nil role resolver")
	}
	s := &TreasuryService{
		store: d.Store, outflows: d.Outflows, alerter: d.Alerter,
		resolve: d.Resolver, now: d.Now, ratio: d.LiquidityRatio,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if !s.ratio.IsPositive() {
		s.ratio = decimal.NewFromInt(1)
	}
	return s, nil
}

func (s *TreasuryService) raise(ctx context.Context, severity, code, summary string, details map[string]string) {
	if s.alerter == nil {
		return
	}
	_ = s.alerter.Raise(ctx, OpsAlert{
		Severity: severity, Code: code, Summary: summary, Details: details,
	})
}

func (s *TreasuryService) requireRole(ctx context.Context, userID int64, allowed map[string]bool) error {
	if userID <= 0 {
		return excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	role, err := s.resolve(ctx, userID)
	if err != nil {
		return excerrors.Wrap("UNAUTHORIZED_ROLE", "role lookup failed", err)
	}
	if !allowed[role] {
		return excerrors.New("UNAUTHORIZED_ROLE",
			fmt.Sprintf("role %q lacks permission for this treasury operation", role))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Own-funds ledger (item 1)
// ---------------------------------------------------------------------------

// SetOwnFunds upserts one house-capital line (Finance Ops). LineKind must
// be one of the four ledger kinds — client money can never land here.
func (s *TreasuryService) SetOwnFunds(ctx context.Context, actor admin.AdminActor, o OwnFunds) (*OwnFunds, error) {
	if err := s.requireRole(ctx, actor.UserID, financeWriteRoles); err != nil {
		return nil, err
	}
	valid := false
	for _, k := range ownFundsKinds {
		if o.LineKind == k {
			valid = true
			break
		}
	}
	if !valid {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("line_kind %q not in the own-funds ledger domain", o.LineKind))
	}
	if len(o.Currency) != 3 || o.Balance.IsNegative() {
		return nil, excerrors.New("INVALID_REQUEST",
			"own-funds lines require a 3-letter currency and non-negative balance")
	}
	o.ReconciliationStatus = ReconPending
	return s.store.UpsertOwnFunds(ctx, o)
}

// ReconcileOwnFunds compares a line against its bank-statement balance
// (daily, Task 24.3.12 seam): match → RECONCILED, divergence → BREAK.
func (s *TreasuryService) ReconcileOwnFunds(ctx context.Context, actor admin.AdminActor,
	lineID int64, statementBalance decimal.Decimal, statementRef string) (*OwnFunds, error) {
	if err := s.requireRole(ctx, actor.UserID, financeWriteRoles); err != nil {
		return nil, err
	}
	var out *OwnFunds
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		o, err := tx.OwnFundsByID(ctx, lineID)
		if err != nil {
			return err
		}
		if o == nil {
			return excerrors.New("NOT_FOUND", "own-funds line not found")
		}
		now := s.now()
		o.StatementBalance = &statementBalance
		o.StatementRef = statementRef
		o.ReconciledAt = &now
		if statementBalance.Equal(o.Balance) {
			o.ReconciliationStatus = ReconReconciled
		} else {
			o.ReconciliationStatus = ReconBreak
		}
		updated, err := tx.UpsertOwnFunds(ctx, *o)
		if err != nil {
			return err
		}
		out = updated
		if out.ReconciliationStatus == ReconBreak {
			s.raise(ctx, SeverityP1, "OWN_FUNDS_RECON_BREAK",
				fmt.Sprintf("own-funds line %d (%s %s) diverges from statement %s",
					lineID, o.LineKind, o.Currency, statementBalance.String()),
				map[string]string{"line_id": fmt.Sprint(lineID)})
		}
		return nil
	})
	return out, err
}

// ListOwnFunds is the read surface (Finance Ops; Read-Only Auditor and
// EXTERNAL_AUDITOR per the breach/coverage visibility clause).
func (s *TreasuryService) ListOwnFunds(ctx context.Context, actor admin.AdminActor) ([]OwnFunds, error) {
	if err := s.requireRole(ctx, actor.UserID, readRoles); err != nil {
		return nil, err
	}
	return s.store.ListOwnFunds(ctx)
}

// HouseReserve implements HouseReserveSource — the liquid leg of the
// Task 24.3.11 stress test and Tier-2 capacity check (equity + reserves +
// retained earnings; the insurance-fund line is deliberately excluded —
// it is a separate waterfall tier, not house money).
func (s *TreasuryService) HouseReserve(ctx context.Context, tx Tx, ccy string) (decimal.Decimal, error) {
	return s.store.OwnFundsSum(ctx, ccy, liquidKinds)
}

// ---------------------------------------------------------------------------
// Insurance & reinsurance cover (item 3)
// ---------------------------------------------------------------------------

// CheckInsuranceExpiries pages INSURANCE_POLICY_EXPIRING (P2) for every
// live policy commitment expiring inside 60 days — once per policy
// (expiry_alerted_at dedups the daily sweep).
func (s *TreasuryService) CheckInsuranceExpiries(ctx context.Context) error {
	horizon := s.now().Add(InsuranceExpiryHorizon)
	return s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		policies, err := tx.ExpiringPolicies(ctx, horizon)
		if err != nil {
			return err
		}
		for i := range policies {
			p := &policies[i]
			if p.ExpiryAlertedAt != nil {
				continue
			}
			s.raise(ctx, SeverityP2, alertInsuranceExpiring,
				fmt.Sprintf("%s cover with %s expires %s (limit %s %s) — renewal required",
					p.PolicyType, p.ProviderName, p.ExpiresAt.Format("2006-01-02"),
					p.CoverLimit.String(), p.Currency),
				map[string]string{
					"commitment_id": fmt.Sprint(p.ID), "provider": p.ProviderName,
					"policy_type": p.PolicyType,
				})
			p.ExpiryAlertedAt = ptrTime(s.now())
			if err := tx.UpdateCommitment(ctx, *p); err != nil {
				return err
			}
		}
		return nil
	})
}

func ptrTime(t time.Time) *time.Time { return &t }

// ---------------------------------------------------------------------------
// Stressed liquidity buffer (item 4)
// ---------------------------------------------------------------------------

// EvaluateLiquidity assesses the stressed 5-business-day buffer for ccy:
// liquid house funds ≥ LiquidityRatio × stressed outflow. BREACH persists
// the assessment, pages P1 and freezes discretionary outflows + LP
// capacity in the same transaction — the freeze outlives the call.
func (s *TreasuryService) EvaluateLiquidity(ctx context.Context, ccy string) (*LiquidityAssessment, error) {
	if s.outflows == nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"stressed-outflow seam not wired — liquidity cannot be evaluated (fail closed)")
	}
	if len(ccy) != 3 {
		return nil, excerrors.New("INVALID_REQUEST", "currency must be 3-letter ISO")
	}
	outflow, err := s.outflows.StressedOutflow5d(ctx, ccy)
	if err != nil {
		return nil, err
	}
	var out *LiquidityAssessment
	err = s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		liquid, err := tx.OwnFundsSum(ctx, ccy, liquidKinds)
		if err != nil {
			return err
		}
		required := outflow.Mul(s.ratio)
		status := "OK"
		if liquid.LessThan(required) {
			status = "BREACH"
		}
		det, _ := json.Marshal(map[string]any{
			"ratio": s.ratio.String(), "kinds": liquidKinds,
		})
		row, err := tx.InsertLiquidityAssessment(ctx, LiquidityAssessment{
			AssessedAt: s.now(), Currency: ccy, LiquidHouseFunds: liquid,
			StressedOutflow5d: outflow, RequiredBuffer: required,
			Status: status, Detail: det,
		})
		if err != nil {
			return err
		}
		out = row
		if status == "BREACH" {
			ctrl, err := tx.TreasuryControls(ctx)
			if err != nil {
				return err
			}
			now := s.now()
			ctrl.DiscretionaryOutflowsFrozen = true
			ctrl.LPCapacityBlocked = true
			ctrl.FrozenAt = &now
			ctrl.FreezeReason = CodeTreasuryLiquidityBreach
			if err := tx.UpdateTreasuryControls(ctx, *ctrl); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out.Status == "BREACH" {
		s.raise(ctx, SeverityP1, alertLiquidityBreach,
			fmt.Sprintf("stressed 5-day liquidity breach %s: liquid %s < required %s",
				ccy, out.LiquidHouseFunds.String(), out.RequiredBuffer.String()),
			map[string]string{"currency": ccy})
	}
	return out, nil
}

// AssertDiscretionaryOutflow gates discretionary house outflows while a
// breach freeze stands — TREASURY_LIQUIDITY_BREACH (HTTP 503).
func (s *TreasuryService) AssertDiscretionaryOutflow(ctx context.Context) error {
	ctrl, err := s.store.TreasuryControls(ctx)
	if err != nil {
		return err
	}
	if ctrl != nil && ctrl.DiscretionaryOutflowsFrozen {
		return excerrors.New(CodeTreasuryLiquidityBreach,
			"discretionary house outflows frozen by stressed-liquidity breach")
	}
	return nil
}

// AssertLPCapacity gates new LP capacity while lp_capacity_blocked stands.
func (s *TreasuryService) AssertLPCapacity(ctx context.Context) error {
	ctrl, err := s.store.TreasuryControls(ctx)
	if err != nil {
		return err
	}
	if ctrl != nil && ctrl.LPCapacityBlocked {
		return excerrors.New(CodeTreasuryLiquidityBreach,
			"new LP capacity blocked by stressed-liquidity breach")
	}
	return nil
}

// LiftLiquidityFreeze clears the breach flags after remediation — dual
// control (initiator ≠ approver), both principals logged on the row.
func (s *TreasuryService) LiftLiquidityFreeze(ctx context.Context, actor admin.AdminActor) (*TreasuryControls, error) {
	if err := s.requireRole(ctx, actor.UserID, financeWriteRoles); err != nil {
		return nil, err
	}
	if actor.ApproverID <= 0 {
		return nil, excerrors.New("DUAL_CONTROL_REQUIRED",
			"lifting a liquidity freeze requires a distinct approver_id")
	}
	if actor.ApproverID == actor.UserID {
		return nil, excerrors.New("DUAL_CONTROL_VIOLATION",
			"approver must differ from the initiating principal")
	}
	if err := s.requireRole(ctx, actor.ApproverID, complianceOrFinance); err != nil {
		return nil, err
	}
	var out *TreasuryControls
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		ctrl, err := tx.TreasuryControls(ctx)
		if err != nil {
			return err
		}
		if ctrl == nil {
			return excerrors.New("INTERNAL_ERROR", "treasury_controls row missing")
		}
		now := s.now()
		ctrl.DiscretionaryOutflowsFrozen = false
		ctrl.LPCapacityBlocked = false
		ctrl.UnfrozenBy = &actor.UserID
		ctrl.UnfrozenAt = &now
		if err := tx.UpdateTreasuryControls(ctx, *ctrl); err != nil {
			return err
		}
		out = ctrl
		return nil
	})
	return out, err
}

// Controls returns the current freeze flags (read surface).
func (s *TreasuryService) Controls(ctx context.Context, actor admin.AdminActor) (*TreasuryControls, error) {
	if err := s.requireRole(ctx, actor.UserID, readRoles); err != nil {
		return nil, err
	}
	return s.store.TreasuryControls(ctx)
}

// ---------------------------------------------------------------------------
// Admission gate (item 6)
// ---------------------------------------------------------------------------

// AdmissionSatisfied is the Phase-21 Task 21.3.13 "minimum financial
// resources" launch-prerequisite seam: TRUE only when the own-funds
// ledger is funded (Σ all lines > 0) AND the contingent-capital waterfall
// terminates in an EXECUTED funded backstop — attestation is never enough.
func (s *TreasuryService) AdmissionSatisfied(ctx context.Context) (bool, error) {
	funded, err := s.store.OwnFundsSum(ctx, "", ownFundsKinds)
	if err != nil {
		return false, err
	}
	if !funded.IsPositive() {
		return false, nil
	}
	return s.BackstopFunded(ctx)
}

// --- PgStore: treasury tables (migration 082) ---------------------------------

const ownFundsCols = `id, line_kind, currency, balance::text,
	reconciliation_status, COALESCE(statement_ref,''), statement_balance::text,
	reconciled_at, updated_at, created_at`

func scanOwnFunds(row pgx.Row) (*OwnFunds, error) {
	var o OwnFunds
	var bal string
	var sb *string
	if err := row.Scan(&o.ID, &o.LineKind, &o.Currency, &bal,
		&o.ReconciliationStatus, &o.StatementRef, &sb, &o.ReconciledAt,
		&o.UpdatedAt, &o.CreatedAt); err != nil {
		return nil, err
	}
	o.Balance = decimal.RequireFromString(bal)
	if sb != nil {
		d := decimal.RequireFromString(*sb)
		o.StatementBalance = &d
	}
	return &o, nil
}

func (s *PgStore) UpsertOwnFunds(ctx context.Context, o OwnFunds) (*OwnFunds, error) {
	var sb any
	if o.StatementBalance != nil {
		sb = o.StatementBalance.String()
	}
	return scanOwnFunds(s.q.QueryRow(ctx, `
		INSERT INTO own_funds_balances
		 (line_kind, currency, balance, reconciliation_status,
		  statement_ref, statement_balance, reconciled_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT (line_kind, currency) DO UPDATE SET
		   balance=EXCLUDED.balance,
		   reconciliation_status=EXCLUDED.reconciliation_status,
		   statement_ref=EXCLUDED.statement_ref,
		   statement_balance=EXCLUDED.statement_balance,
		   reconciled_at=EXCLUDED.reconciled_at,
		   updated_at=now()
		 RETURNING `+ownFundsCols,
		o.LineKind, o.Currency, o.Balance.String(), o.ReconciliationStatus,
		nilIfEmpty(o.StatementRef), sb, o.ReconciledAt))
}

func (s *PgStore) OwnFundsByID(ctx context.Context, id int64) (*OwnFunds, error) {
	o, err := scanOwnFunds(s.q.QueryRow(ctx,
		`SELECT `+ownFundsCols+` FROM own_funds_balances WHERE id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return o, err
}

func (s *PgStore) ListOwnFunds(ctx context.Context) ([]OwnFunds, error) {
	rows, err := s.q.Query(ctx,
		`SELECT `+ownFundsCols+` FROM own_funds_balances ORDER BY line_kind, currency`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OwnFunds{}
	for rows.Next() {
		var o OwnFunds
		var bal string
		var sb *string
		if err := rows.Scan(&o.ID, &o.LineKind, &o.Currency, &bal,
			&o.ReconciliationStatus, &o.StatementRef, &sb, &o.ReconciledAt,
			&o.UpdatedAt, &o.CreatedAt); err != nil {
			return nil, err
		}
		o.Balance = decimal.RequireFromString(bal)
		if sb != nil {
			d := decimal.RequireFromString(*sb)
			o.StatementBalance = &d
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OwnFundsSum aggregates balances across the given line kinds; ccy ""
// sums every currency (admission-gate view), otherwise per-currency.
func (s *PgStore) OwnFundsSum(ctx context.Context, ccy string, kinds []string) (decimal.Decimal, error) {
	if len(kinds) == 0 {
		return decimal.Zero, nil
	}
	q := `SELECT COALESCE(sum(balance),0)::text FROM own_funds_balances WHERE line_kind = ANY($1)`
	args := []any{kinds}
	if ccy != "" {
		args = append(args, ccy)
		q += ` AND currency=$2`
	}
	var total *string
	err := s.q.QueryRow(ctx, q, args...).Scan(&total)
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.RequireFromString(*total), nil
}

func (s *PgStore) InsertLiquidityAssessment(ctx context.Context, a LiquidityAssessment) (*LiquidityAssessment, error) {
	var out LiquidityAssessment
	var liq, outf, req string
	err := s.q.QueryRow(ctx, `
		INSERT INTO treasury_liquidity_assessments
		 (assessed_at, currency, liquid_house_funds, stressed_outflow_5d,
		  required_buffer, status, detail)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 RETURNING id, assessed_at, currency, liquid_house_funds::text,
		           stressed_outflow_5d::text, required_buffer::text, status,
		           detail, created_at`,
		a.AssessedAt, a.Currency, a.LiquidHouseFunds.String(),
		a.StressedOutflow5d.String(), a.RequiredBuffer.String(), a.Status, a.Detail).
		Scan(&out.ID, &out.AssessedAt, &out.Currency, &liq, &outf, &req,
			&out.Status, &out.Detail, &out.CreatedAt)
	if err != nil {
		return nil, err
	}
	out.LiquidHouseFunds = decimal.RequireFromString(liq)
	out.StressedOutflow5d = decimal.RequireFromString(outf)
	out.RequiredBuffer = decimal.RequireFromString(req)
	return &out, nil
}

func (s *PgStore) TreasuryControls(ctx context.Context) (*TreasuryControls, error) {
	var c TreasuryControls
	err := s.q.QueryRow(ctx, `
		SELECT discretionary_outflows_frozen, lp_capacity_blocked,
		       frozen_at, COALESCE(freeze_reason,''), unfrozen_by, unfrozen_at
		  FROM treasury_controls WHERE id=1`).
		Scan(&c.DiscretionaryOutflowsFrozen, &c.LPCapacityBlocked,
			&c.FrozenAt, &c.FreezeReason, &c.UnfrozenBy, &c.UnfrozenAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &c, err
}

func (s *PgStore) UpdateTreasuryControls(ctx context.Context, c TreasuryControls) error {
	_, err := s.q.Exec(ctx, `
		UPDATE treasury_controls
		   SET discretionary_outflows_frozen=$2, lp_capacity_blocked=$3,
		       frozen_at=$4, freeze_reason=$5, unfrozen_by=$6, unfrozen_at=$7
		 WHERE id=1`,
		c.DiscretionaryOutflowsFrozen, c.LPCapacityBlocked, c.FrozenAt,
		nilIfEmpty(c.FreezeReason), c.UnfrozenBy, c.UnfrozenAt)
	return err
}
