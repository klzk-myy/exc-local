// Basel III capital adequacy & leverage ratio reporting — Phase-21 Task
// 21.3.13 (spec §14.1; §24 #151; §27.1 Basel III matrix row →
// CAPITAL_ADEQUACY_BREACH / LEVERAGE_RATIO_BREACH).
//
// Inputs come from the Phase-03 general ledger (chart_of_accounts /
// ledger_lines, migrations 036/088) plus the settlement/PB exposure
// tables — the entity's own prudential numbers, not client math:
//
//	Tier 1  = house equity (3xxx credit balances) + current-period
//	          retained earnings (4xxx revenue − 5xxx expense).
//	Tier 2  = insurance-fund reserve (2210_INSURANCE_FUND_LIABILITY_*
//	          credit balance) — the general-reserve analogue.
//	RWA     = counterparty credit risk on open settlement exposure
//	          (PENDING settlement_instructions × 100% weight) + PB
//	          margin (pb_credit_limits NOP + DSL counters, already
//	          USD-equivalent). A deliberately conservative
//	          approximation — documented in inputs.reconciliation.
//	Leverage exposure = house assets (1xxx nostro/clearing balances,
//	          client-segregated 1110 and restricted 1150 excluded) +
//	          the same off-balance exposures.
//
// Ratios: CAR = total capital / RWA (floor 8%), leverage ratio = Tier 1
// / exposure (floor 3%). Breaches page P1 to Compliance Officer +
// Finance Ops through the ops-alerter seam and carry the §27.1 matrix
// codes on the stored row + handler payload — never silently green.
//
// Reporting currency: all components price into reporting_currency
// (default USD) through the RateConverter seam. A component that cannot
// be priced is EXCLUDED from the totals and flips inputs_complete
// false — understating capital is the pessimistic direction (a breach
// can only over-trigger), and the flag keeps the gap visible. The EOD
// sweep is idempotent on snapshot_key 'eod:{period}'; officer-triggered
// regeneration uses 'regen:{token}' so every version is retained.
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

// Basel ratio floors (spec §24 #151).
var (
	BaselCARFloor      = decimal.RequireFromString("0.08")
	BaselLeverageFloor = decimal.RequireFromString("0.03")
)

// baselCodes are the §27.1 matrix codes the report payload + alerts
// carry — registered in errs localCodes with this task as owner.
const (
	BaselCodeCapitalBreach  = "CAPITAL_ADEQUACY_BREACH"
	BaselCodeLeverageBreach = "LEVERAGE_RATIO_BREACH"
)

// BaselReport is one basel_reports row.
type BaselReport struct {
	ID                int64           `json:"id"`
	Period            time.Time       `json:"period"`
	ReportingCurrency string          `json:"reporting_currency"`
	Tier1Capital      decimal.Decimal `json:"tier1_capital"`
	Tier2Capital      decimal.Decimal `json:"tier2_capital"`
	TotalCapital      decimal.Decimal `json:"total_capital"`
	RWA               decimal.Decimal `json:"rwa"`
	LeverageExposure  decimal.Decimal `json:"leverage_exposure"`
	CAR               decimal.Decimal `json:"car"`
	LeverageRatio     decimal.Decimal `json:"leverage_ratio"`
	CARBreach         bool            `json:"car_breach"`
	LeverageBreach    bool            `json:"leverage_breach"`
	InputsComplete    bool            `json:"inputs_complete"`
	Inputs            json.RawMessage `json:"inputs"`
	SnapshotKey       string          `json:"snapshot_key"`
	GeneratedBy       string          `json:"generated_by"`
	CreatedAt         time.Time       `json:"created_at"`
	// Code mirrors the AML program-status convention — populated when a
	// breach flag stands so the wire payload carries the matrix code.
	Code string `json:"code,omitempty"`
}

// BaselRateConverter prices 1 unit of a currency into the reporting
// currency (mark-mid in production). nil → only same-currency
// components price; the rest flag inputs_complete=false.
type BaselRateConverter func(ctx context.Context, ccy string) (decimal.Decimal, error)

// BaselService computes and stores the report versions.
type BaselService struct {
	pool         *pgxpool.Pool
	conv         BaselRateConverter
	reportingCCY string
	alerter      HoldAlerter
	now          func() time.Time
}

// NewBaselService wires the service; the pool is required.
func NewBaselService(pool *pgxpool.Pool) (*BaselService, error) {
	if pool == nil {
		return nil, excerrors.New("INTERNAL_ERROR", "basel service requires pool")
	}
	return &BaselService{pool: pool, reportingCCY: "USD", now: time.Now}, nil
}

// WithRateConverter wires the FX conversion seam for non-reporting-ccy
// components.
func (s *BaselService) WithRateConverter(c BaselRateConverter) *BaselService {
	s.conv = c
	return s
}

// WithReportingCurrency overrides the reporting currency (default USD).
func (s *BaselService) WithReportingCurrency(ccy string) *BaselService {
	if len(ccy) == 3 {
		s.reportingCCY = ccy
	}
	return s
}

// WithAlerter wires the ops channel for breach pages.
func (s *BaselService) WithAlerter(a HoldAlerter) *BaselService {
	s.alerter = a
	return s
}

// WithClock overrides the clock (tests).
func (s *BaselService) WithClock(c func() time.Time) *BaselService {
	s.now = c
	return s
}

// ---------------------------------------------------------------------------
// Input collection
// ---------------------------------------------------------------------------

// glBalance is one chart-of-accounts line's net balance.
type glBalance struct {
	AccountCode string
	AccountType string
	Currency    string
	Net         decimal.Decimal // credit-norm: credits − debits
}

// glBalances rolls every posted ledger line up to its account code.
func (s *BaselService) glBalances(ctx context.Context) ([]glBalance, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT l.account_code, c.account_type::text, l.currency,
		       (coalesce(sum(l.credit_amount),0) - coalesce(sum(l.debit_amount),0))::text
		  FROM ledger_lines l
		  JOIN chart_of_accounts c ON c.account_code = l.account_code
		 GROUP BY l.account_code, c.account_type, l.currency`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "gl rollup", err)
	}
	defer rows.Close()
	var out []glBalance
	for rows.Next() {
		var b glBalance
		var net *string
		if err := rows.Scan(&b.AccountCode, &b.AccountType, &b.Currency, &net); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "gl scan", err)
		}
		b.Net = decOr(net)
		out = append(out, b)
	}
	return out, rows.Err()
}

// openSettlementExposure is the counterparty-credit exposure on
// settlement instructions not yet settled or reconciled.
func (s *BaselService) openSettlementExposure(ctx context.Context) (map[string]decimal.Decimal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT currency, coalesce(sum(amount),0)::text
		  FROM settlement_instructions
		 WHERE status = 'PENDING'
		 GROUP BY currency`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "settlement exposure", err)
	}
	defer rows.Close()
	out := map[string]decimal.Decimal{}
	for rows.Next() {
		var ccy, amt string
		if err := rows.Scan(&ccy, &amt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "settlement scan", err)
		}
		out[ccy] = decOr(&amt)
	}
	return out, rows.Err()
}

// pbMarginUsage is the USD-equivalent PB credit drawn — NOP and DSL
// counters are already maintained in USD by the Phase-19 pre-trade
// path (migration 037 header note).
func (s *BaselService) pbMarginUsage(ctx context.Context) (decimal.Decimal, error) {
	var tot *string
	err := s.pool.QueryRow(ctx, `
		SELECT coalesce(sum(current_net_open_position + current_daily_settled),0)::text
		  FROM pb_credit_limits`).Scan(&tot)
	if err != nil {
		return decimal.Zero, excerrors.Wrap("INTERNAL_ERROR", "pb margin", err)
	}
	return decOr(tot), nil
}

// convert prices one amount into the reporting currency. Returns
// (priced, true) or (zero, false) — false marks the input incomplete.
func (s *BaselService) convert(ctx context.Context, amt decimal.Decimal, ccy string,
	incomplete *[]string) (decimal.Decimal, bool) {
	if !amt.IsPositive() {
		return decimal.Zero, true // zero needs no rate
	}
	if ccy == s.reportingCCY {
		return amt, true
	}
	if s.conv == nil {
		*incomplete = append(*incomplete,
			fmt.Sprintf("no rate converter for %s", ccy))
		return decimal.Zero, false
	}
	rate, err := s.conv(ctx, ccy)
	if err != nil || !rate.IsPositive() {
		*incomplete = append(*incomplete,
			fmt.Sprintf("no %s/%s rate", ccy, s.reportingCCY))
		return decimal.Zero, false
	}
	return amt.Mul(rate), true
}

// Compute assembles the inputs, derives the ratios and returns the
// report (unpersisted — Snapshot stores it).
func (s *BaselService) Compute(ctx context.Context, period time.Time) (*BaselReport, error) {
	bals, err := s.glBalances(ctx)
	if err != nil {
		return nil, err
	}
	settle, err := s.openSettlementExposure(ctx)
	if err != nil {
		return nil, err
	}
	pb, err := s.pbMarginUsage(ctx)
	if err != nil {
		return nil, err
	}

	var incomplete []string
	tier1, tier2 := decimal.Zero, decimal.Zero
	houseAssets := decimal.Zero
	recon := map[string]any{"accounts": []map[string]string{}}
	var acctRows []map[string]string
	for _, b := range bals {
		switch {
		case b.AccountType == "EQUITY", b.AccountType == "REVENUE":
			v, ok := s.convert(ctx, b.Net, b.Currency, &incomplete)
			if ok {
				tier1 = tier1.Add(v)
			}
		case b.AccountType == "EXPENSE":
			// Expense lines are debit-positive: Net is negative and
			// reduces retained earnings inside Tier 1.
			v, ok := s.convert(ctx, b.Net, b.Currency, &incomplete)
			if ok {
				tier1 = tier1.Add(v)
			}
		case len(b.AccountCode) >= 4 && b.AccountCode[:4] == "2210":
			// Insurance fund reserve → Tier 2 general-reserve analogue.
			v, ok := s.convert(ctx, b.Net, b.Currency, &incomplete)
			if ok {
				tier2 = tier2.Add(v)
			}
		case b.AccountType == "ASSET":
			// Leverage exposure counts house assets only: 1110
			// client-segregated money and 1150 restricted insurance
			// nostro are not exposure measure components.
			if len(b.AccountCode) >= 4 && (b.AccountCode[:4] == "1110" || b.AccountCode[:4] == "1150") {
				break
			}
			v, ok := s.convert(ctx, b.Net, b.Currency, &incomplete)
			if ok {
				houseAssets = houseAssets.Add(v)
			}
		}
		acctRows = append(acctRows, map[string]string{
			"account_code": b.AccountCode, "type": b.AccountType,
			"currency": b.Currency, "net": b.Net.String(),
		})
	}
	recon["accounts"] = acctRows

	settleExp := decimal.Zero
	settleDetail := map[string]string{}
	for ccy, amt := range settle {
		v, ok := s.convert(ctx, amt, ccy, &incomplete)
		if ok {
			settleExp = settleExp.Add(v)
		}
		settleDetail[ccy] = amt.String()
	}
	// PB counters are USD-equivalent (migration 037); convert only when
	// the reporting currency differs.
	pbExp := pb
	if pb.IsPositive() && s.reportingCCY != "USD" {
		v, ok := s.convert(ctx, pb, "USD", &incomplete)
		if ok {
			pbExp = v
		} else {
			pbExp = decimal.Zero
		}
	}

	// RWA: 100% counterparty weight on open settlement + PB margin —
	// the documented approximation (a true SA-CCR decomposition needs
	// per-counterparty netting sets Phase-24 reconciles).
	rwa := settleExp.Add(pbExp)
	levExp := houseAssets.Add(settleExp).Add(pbExp)
	total := tier1.Add(tier2)

	var car, lev decimal.Decimal
	if rwa.IsPositive() {
		car = total.Div(rwa)
	}
	if levExp.IsPositive() {
		lev = tier1.Div(levExp)
	}
	rep := &BaselReport{
		Period:            period,
		ReportingCurrency: s.reportingCCY,
		Tier1Capital:      tier1, Tier2Capital: tier2, TotalCapital: total,
		RWA: rwa, LeverageExposure: levExp,
		CAR: car, LeverageRatio: lev,
		CARBreach:      car.LessThan(BaselCARFloor),
		LeverageBreach: lev.LessThan(BaselLeverageFloor),
		InputsComplete: len(incomplete) == 0,
	}
	if rep.CARBreach {
		rep.Code = BaselCodeCapitalBreach
	} else if rep.LeverageBreach {
		rep.Code = BaselCodeLeverageBreach
	}
	recon["settlement_exposure_by_ccy"] = settleDetail
	recon["pb_margin_usd"] = pb.String()
	recon["model"] = "T1=3xxx+4xxx-5xxx; T2=2210 insurance reserve; " +
		"RWA=settlement+PB at 100% CCR weight; levExp=house assets + exposures"
	if len(incomplete) > 0 {
		recon["unpriced_inputs"] = incomplete
	}
	inputs, _ := json.Marshal(recon)
	rep.Inputs = inputs
	return rep, nil
}

// Snapshot persists a computed report. snapshotKey dedups reruns — a
// conflicting insert replays the stored row with created=false.
func (s *BaselService) Snapshot(ctx context.Context, period time.Time,
	snapshotKey, generatedBy string) (*BaselReport, bool, error) {
	if snapshotKey == "" {
		return nil, false, excerrors.New("INVALID_REQUEST", "snapshot_key required")
	}
	rep, err := s.Compute(ctx, period)
	if err != nil {
		return nil, false, err
	}
	rep.SnapshotKey = snapshotKey
	rep.GeneratedBy = generatedBy

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "basel tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO basel_reports
		    (period, reporting_currency, tier1_capital, tier2_capital,
		     total_capital, rwa, leverage_exposure, car, leverage_ratio,
		     car_breach, leverage_breach, inputs_complete, inputs,
		     snapshot_key, generated_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (snapshot_key) DO NOTHING
		RETURNING id`,
		rep.Period, rep.ReportingCurrency, rep.Tier1Capital, rep.Tier2Capital,
		rep.TotalCapital, rep.RWA, rep.LeverageExposure, rep.CAR,
		rep.LeverageRatio, rep.CARBreach, rep.LeverageBreach,
		rep.InputsComplete, rep.Inputs, rep.SnapshotKey,
		rep.GeneratedBy).Scan(&id)
	if err == pgx.ErrNoRows {
		existing, gerr := s.getByKey(ctx, snapshotKey)
		if gerr != nil {
			return nil, false, gerr
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, excerrors.Wrap("INTERNAL_ERROR", "basel dedup commit", err)
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "basel insert", err)
	}
	if _, err := audit.Append(ctx, tx, "basel_reports", &id,
		"BASEL_SNAPSHOT", nil); err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "basel audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "basel commit", err)
	}
	rep.ID = id
	rep.CreatedAt = s.now().UTC()
	s.raiseBreaches(ctx, rep)
	return rep, true, nil
}

// RunEOD snapshots yesterday's UTC close — the daily sweep entry point
// (idempotent on 'eod:{period}').
func (s *BaselService) RunEOD(ctx context.Context) (*BaselReport, bool, error) {
	yesterday := s.now().UTC().Add(-24 * time.Hour)
	period := time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(),
		0, 0, 0, 0, time.UTC)
	return s.Snapshot(ctx, period, "eod:"+period.Format("2006-01-02"), "eod-sweep")
}

// raiseBreaches pages P1 on threshold violations — never a silent
// dashboard flag (Compliance Officer + Finance Ops per the task).
func (s *BaselService) raiseBreaches(ctx context.Context, rep *BaselReport) {
	if s.alerter == nil {
		return
	}
	if rep.CARBreach {
		_ = s.alerter.RaiseHold(ctx, HoldAlert{
			Severity: "P1", Code: BaselCodeCapitalBreach,
			Summary: fmt.Sprintf("Basel III CAR %s below %s floor (period %s)",
				rep.CAR.StringFixed(4), BaselCARFloor.StringFixed(2),
				rep.Period.Format("2006-01-02")),
			Details: map[string]string{
				"tier1": rep.Tier1Capital.String(), "total": rep.TotalCapital.String(),
				"rwa": rep.RWA.String(), "inputs_complete": fmt.Sprint(rep.InputsComplete),
			},
		})
	}
	if rep.LeverageBreach {
		_ = s.alerter.RaiseHold(ctx, HoldAlert{
			Severity: "P1", Code: BaselCodeLeverageBreach,
			Summary: fmt.Sprintf("Basel III leverage ratio %s below %s floor (period %s)",
				rep.LeverageRatio.StringFixed(4), BaselLeverageFloor.StringFixed(2),
				rep.Period.Format("2006-01-02")),
			Details: map[string]string{
				"tier1":    rep.Tier1Capital.String(),
				"exposure": rep.LeverageExposure.String(),
			},
		})
	}
	if !rep.InputsComplete {
		_ = s.alerter.RaiseHold(ctx, HoldAlert{
			Severity: "P1", Code: BaselCodeCapitalBreach,
			Summary: fmt.Sprintf("Basel III report %d has unpriced inputs (fail-closed flag)",
				rep.ID),
		})
	}
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

const baselCols = `
	id, period, reporting_currency, tier1_capital::text, tier2_capital::text,
	total_capital::text, rwa::text, leverage_exposure::text, car::text,
	leverage_ratio::text, car_breach, leverage_breach, inputs_complete,
	inputs, snapshot_key, generated_by, created_at`

func scanBasel(row rowScanner) (*BaselReport, error) {
	var r BaselReport
	var t1, t2, tot, rwa, lev, car, lr *string
	err := row.Scan(&r.ID, &r.Period, &r.ReportingCurrency, &t1, &t2,
		&tot, &rwa, &lev, &car, &lr, &r.CARBreach, &r.LeverageBreach,
		&r.InputsComplete, &r.Inputs, &r.SnapshotKey, &r.GeneratedBy,
		&r.CreatedAt)
	if err != nil {
		return nil, err
	}
	r.Tier1Capital = decOr(t1)
	r.Tier2Capital = decOr(t2)
	r.TotalCapital = decOr(tot)
	r.RWA = decOr(rwa)
	r.LeverageExposure = decOr(lev)
	r.CAR = decOr(car)
	r.LeverageRatio = decOr(lr)
	if r.CARBreach {
		r.Code = BaselCodeCapitalBreach
	} else if r.LeverageBreach {
		r.Code = BaselCodeLeverageBreach
	}
	return &r, nil
}

func (s *BaselService) getByKey(ctx context.Context, key string) (*BaselReport, error) {
	rep, err := scanBasel(s.pool.QueryRow(ctx,
		`SELECT `+baselCols+` FROM basel_reports WHERE snapshot_key = $1`, key))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("INTERNAL_ERROR",
			"basel dedup conflict without stored row")
	}
	return rep, err
}

// Get returns the newest report for a period (date-truncated), or the
// newest report overall when period is zero.
func (s *BaselService) Get(ctx context.Context, period time.Time) (*BaselReport, error) {
	var row rowScanner
	if period.IsZero() {
		row = s.pool.QueryRow(ctx,
			`SELECT `+baselCols+` FROM basel_reports ORDER BY period DESC, id DESC LIMIT 1`)
	} else {
		row = s.pool.QueryRow(ctx,
			`SELECT `+baselCols+` FROM basel_reports WHERE period = $1
			 ORDER BY id DESC LIMIT 1`, period)
	}
	rep, err := scanBasel(row)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "no basel report for period")
	}
	return rep, err
}

// List returns newest-first report versions for the audit surface.
func (s *BaselService) List(ctx context.Context, limit int) ([]BaselReport, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+baselCols+` FROM basel_reports
		 ORDER BY period DESC, id DESC LIMIT `+fmt.Sprint(limit))
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "basel list", err)
	}
	defer rows.Close()
	var out []BaselReport
	for rows.Next() {
		var r BaselReport
		var t1, t2, tot, rwa, lev, car, lr *string
		if err := rows.Scan(&r.ID, &r.Period, &r.ReportingCurrency, &t1,
			&t2, &tot, &rwa, &lev, &car, &lr, &r.CARBreach,
			&r.LeverageBreach, &r.InputsComplete, &r.Inputs,
			&r.SnapshotKey, &r.GeneratedBy, &r.CreatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "basel scan", err)
		}
		r.Tier1Capital = decOr(t1)
		r.Tier2Capital = decOr(t2)
		r.TotalCapital = decOr(tot)
		r.RWA = decOr(rwa)
		r.LeverageExposure = decOr(lev)
		r.CAR = decOr(car)
		r.LeverageRatio = decOr(lr)
		out = append(out, r)
	}
	return out, rows.Err()
}
