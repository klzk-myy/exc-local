package risk

// Phase-3 Task 5 (IMP-PLAN) — insurance-fund replenishment sweep
// (Task 19.3.14, spec §13.6b): the scheduled governance waterfall that
// tops the fund back to target. Per governed currency, in order:
//
//  1. Recompute the target — max(min_capital, regulatory_floor,
//     target_pct × client equity) — persisted to
//     insurance_fund_governance.target_balance.
//  2. Daily fee-revenue sweep: credit min(shortfall, fee revenue ×
//     replenishment_fee_pct) as RETAINED_EARNINGS_SWEEP (DR 3020
//     retained earnings / CR 2210 fund liability — journalled by the
//     fund service).
//  3. Below-depletion emergencies draw the committed contingent
//     facility within its cap (CONTINGENT_FACILITY).
//  4. Any residual shortfall becomes a PENDING_APPROVAL
//     insurance_fund_adjustments row (CAPITAL_INJECTION — the §8.2
//     four-eyes gate keeps house-capital moves human-approved) plus a
//     P1 ops alert.
//
// ADL stays last resort at fill time (liquidation RecordFill path) —
// the sweep never deleverages clients to pre-fund the reserve.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// FundReplenisher runs the daily governance waterfall.
type FundReplenisher struct {
	pool    *pgxpool.Pool
	fund    *InsuranceFundService
	alerter OpsAlerter
	now     func() time.Time
	logf    func(format string, args ...any)
}

// NewFundReplenisher wires the sweep. A nil pool/fund fails closed —
// an unpostable sweep is a defect, not a degraded job (§2.7).
func NewFundReplenisher(pool *pgxpool.Pool, fund *InsuranceFundService,
	alerter OpsAlerter) (*FundReplenisher, error) {
	if pool == nil {
		return nil, fmt.Errorf("fund replenisher: nil pgx pool")
	}
	if fund == nil {
		return nil, fmt.Errorf("fund replenisher: nil fund service")
	}
	return &FundReplenisher{pool: pool, fund: fund, alerter: alerter,
		now:  func() time.Time { return time.Now().UTC() },
		logf: func(string, ...any) {},
	}, nil
}

// fundGovernance mirrors one insurance_fund_governance row.
type fundGovernance struct {
	Currency     string
	MinCapital   decimal.Decimal
	TargetPct    decimal.Decimal
	Target       *decimal.Decimal
	FeePct       decimal.Decimal
	RegFloor     decimal.Decimal
	FacilityCap  decimal.Decimal
	LastComputed *time.Time
}

// ReplenishReport summarizes one currency's sweep outcome.
type ReplenishReport struct {
	Currency       string
	Target         decimal.Decimal
	BalanceBefore  decimal.Decimal
	FeeSwept       decimal.Decimal
	FacilityDrawn  decimal.Decimal
	ResidualGap    decimal.Decimal
	ApprovalQueued bool
}

// SweepOnce evaluates every governed currency. A currency's failure
// logs and continues — one broken row must not starve the others; the
// count of moved currencies returns for observability.
func (r *FundReplenisher) SweepOnce(ctx context.Context) ([]ReplenishReport, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT currency, min_capital::text, target_pct_of_equity::text,
		       target_balance::text, replenishment_fee_pct::text,
		       regulatory_floor::text, contingent_facility_cap::text,
		       last_target_computed_at
		FROM insurance_fund_governance ORDER BY currency`)
	if err != nil {
		return nil, fmt.Errorf("fund governance read: %w", err)
	}
	var govs []fundGovernance
	for rows.Next() {
		var g fundGovernance
		var minCap, pct, feePct, floor, cap_ string
		var target *string
		if err := rows.Scan(&g.Currency, &minCap, &pct, &target,
			&feePct, &floor, &cap_, &g.LastComputed); err != nil {
			rows.Close()
			return nil, fmt.Errorf("fund governance scan: %w", err)
		}
		g.MinCapital = decimal.RequireFromString(minCap)
		g.TargetPct = decimal.RequireFromString(pct)
		if target != nil {
			t := decimal.RequireFromString(*target)
			g.Target = &t
		}
		g.FeePct = decimal.RequireFromString(feePct)
		g.RegFloor = decimal.RequireFromString(floor)
		g.FacilityCap = decimal.RequireFromString(cap_)
		govs = append(govs, g)
	}
	rows.Close()

	var out []ReplenishReport
	var firstErr error
	for _, g := range govs {
		rep, err := r.sweepCurrency(ctx, g)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			r.logf("fund replenishment %s failed: %v", g.Currency, err)
			continue
		}
		if rep != nil {
			out = append(out, *rep)
		}
	}
	return out, firstErr
}

// sweepCurrency runs the waterfall for one governed currency.
func (r *FundReplenisher) sweepCurrency(ctx context.Context,
	g fundGovernance) (*ReplenishReport, error) {

	// 1. Target = max(min_capital, regulatory_floor, pct × equity).
	equity, err := r.clientEquity(ctx, g.Currency)
	if err != nil {
		return nil, err
	}
	target := g.MinCapital
	if g.RegFloor.GreaterThan(target) {
		target = g.RegFloor
	}
	if pctTarget := equity.Mul(g.TargetPct); pctTarget.GreaterThan(target) {
		target = pctTarget
	}
	if _, err := r.pool.Exec(ctx, `
		UPDATE insurance_fund_governance
		SET target_balance=$2, last_target_computed_at=now(), updated_at=now()
		WHERE currency=$1`, g.Currency, target); err != nil {
		return nil, fmt.Errorf("fund target persist %s: %w", g.Currency, err)
	}

	bal, err := r.fund.Balance(ctx, g.Currency)
	if err != nil {
		return nil, err
	}
	balance := decimal.Zero
	if bal != nil {
		balance = bal.Balance
	}
	shortfall := target.Sub(balance)
	if !shortfall.IsPositive() {
		return &ReplenishReport{Currency: g.Currency, Target: target,
			BalanceBefore: balance}, nil // at/above target — done
	}

	rep := &ReplenishReport{Currency: g.Currency, Target: target,
		BalanceBefore: balance}
	day := r.now().Format("2006-01-02")

	// 2. Fee-revenue sweep (bounded share of today's trading/commission
	//    revenue — never more than the day's take).
	feeRev, err := r.dailyFeeRevenue(ctx, g.Currency)
	if err != nil {
		return nil, err
	}
	sweep := feeRev.Mul(g.FeePct)
	if sweep.GreaterThan(shortfall) {
		sweep = shortfall
	}
	if sweep.IsPositive() {
		if _, err := r.fund.Credit(ctx, FundMovement{
			Reason:        FundReasonRetainedEarnings,
			Currency:      g.Currency,
			Amount:        sweep,
			ReferenceType: "sweep",
			IdempotencyKey: fmt.Sprintf("fund-sweep:%s:%s",
				g.Currency, day),
			Narrative: fmt.Sprintf("daily fee sweep toward %s target %s",
				g.Currency, target.String()),
		}); err != nil {
			return nil, fmt.Errorf("fund fee sweep %s: %w", g.Currency, err)
		}
		rep.FeeSwept = sweep
		shortfall = shortfall.Sub(sweep)
		balance = balance.Add(sweep)
	}

	// 3. Contingent facility draw — only when the fund sits below the
	//    depletion floor (the §13.6 emergency, not routine top-up).
	if shortfall.IsPositive() {
		depleted, _, derr := r.fund.Depleted(ctx, g.Currency)
		if derr != nil {
			return nil, derr
		}
		if depleted && g.FacilityCap.IsPositive() {
			drawn, err := r.facilityDrawn(ctx, g.Currency)
			if err != nil {
				return nil, err
			}
			avail := g.FacilityCap.Sub(drawn)
			draw := shortfall
			if draw.GreaterThan(avail) {
				draw = avail
			}
			if draw.IsPositive() {
				if _, err := r.fund.Credit(ctx, FundMovement{
					Reason:        FundReasonContingentFacility,
					Currency:      g.Currency,
					Amount:        draw,
					ReferenceType: "sweep",
					IdempotencyKey: fmt.Sprintf("fund-facility:%s:%s",
						g.Currency, day),
					Narrative: fmt.Sprintf("contingent facility draw — fund below depletion floor"),
				}); err != nil {
					return nil, fmt.Errorf("fund facility draw %s: %w", g.Currency, err)
				}
				rep.FacilityDrawn = draw
				shortfall = shortfall.Sub(draw)
			}
		}
	}

	// 4. Residual shortfall → four-eyes approval request + P1 alert.
	if shortfall.IsPositive() {
		rep.ResidualGap = shortfall
		// Idempotent per (ccy, day): a second sweep while one request is
		// open must not double-queue capital calls.
		var pending int
		if err := r.pool.QueryRow(ctx, `
			SELECT count(*) FROM insurance_fund_adjustments
			WHERE currency=$1 AND status='PENDING_APPROVAL'
			  AND created_at >= date_trunc('day', now())`,
			g.Currency).Scan(&pending); err != nil {
			return nil, fmt.Errorf("fund pending-check %s: %w", g.Currency, err)
		}
		if pending == 0 {
			if _, err := r.pool.Exec(ctx, `
				INSERT INTO insurance_fund_adjustments
				    (currency, direction, amount, reason, status, initiated_by)
				VALUES ($1,'CREDIT',$2,$3,'PENDING_APPROVAL',0)`,
				g.Currency, shortfall,
				fmt.Sprintf("replenishment sweep %s — fund %s below target %s",
					day, balance.String(), target.String())); err != nil {
				return nil, fmt.Errorf("fund adjustment request %s: %w", g.Currency, err)
			}
			rep.ApprovalQueued = true
		}
		if r.alerter != nil {
			_ = r.alerter.Raise(ctx, OpsAlert{
				Severity: "P1", Code: "INSURANCE_FUND_BELOW_TARGET",
				Summary: fmt.Sprintf("%s fund %s below target %s; residual %s queued for dual-control approval",
					g.Currency, balance.String(), target.String(), shortfall.String())})
		}
	}
	return rep, nil
}

// clientEquity sums client-liability ledger balances (credit-norm: a
// liability's balance is credits minus debits across its sub-accounts).
func (r *FundReplenisher) clientEquity(ctx context.Context,
	ccy string) (decimal.Decimal, error) {
	var s *string
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(credit_amount - debit_amount),0)::text
		FROM ledger_lines
		WHERE currency=$1 AND account_code LIKE '2010%'`, ccy).Scan(&s)
	if err != nil {
		if err == pgx.ErrNoRows {
			return decimal.Zero, nil
		}
		return decimal.Zero, fmt.Errorf("client equity read %s: %w", ccy, err)
	}
	return decimal.RequireFromString(*s), nil
}

// dailyFeeRevenue sums today's credits into the 4xxx revenue accounts
// for the currency — the replenishment_fee_pct base.
func (r *FundReplenisher) dailyFeeRevenue(ctx context.Context,
	ccy string) (decimal.Decimal, error) {
	var s *string
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(l.credit_amount - l.debit_amount),0)::text
		FROM ledger_lines l
		JOIN journal_entries j ON j.id = l.journal_entry_id
		WHERE l.currency=$1 AND l.account_code LIKE '4%'
		  AND j.posted_at >= date_trunc('day', now())`, ccy).Scan(&s)
	if err != nil {
		if err == pgx.ErrNoRows {
			return decimal.Zero, nil
		}
		return decimal.Zero, fmt.Errorf("fee revenue read %s: %w", ccy, err)
	}
	return decimal.RequireFromString(*s), nil
}

// facilityDrawn totals prior CONTINGENT_FACILITY credits — the draw
// counts against the committed cap until repaid.
func (r *FundReplenisher) facilityDrawn(ctx context.Context,
	ccy string) (decimal.Decimal, error) {
	var s *string
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount),0)::text FROM insurance_fund_transactions
		WHERE currency=$1 AND reason='CONTINGENT_FACILITY' AND direction='CREDIT'`,
		ccy).Scan(&s)
	if err != nil {
		if err == pgx.ErrNoRows {
			return decimal.Zero, nil
		}
		return decimal.Zero, fmt.Errorf("facility drawn read %s: %w", ccy, err)
	}
	return decimal.RequireFromString(*s), nil
}
