package reconciliation

// check_funding.go — FUNDING (category 5), SETTLEMENT (category 6) and
// FEES (category 7).
//
// FUNDING legs (spec §5.6 + §17.16): a COMPLETED withdrawal must carry a
// terminal rail-payment disposition (SETTLED|ACKNOWLEDGED; DISPATCHED is
// in-flight → INCONCLUSIVE); a CONFIRMED/COMPLETED deposit still
// quarantined in suspense is a state contradiction → MISMATCH; the
// bank-statement leg is the StatementSource seam — unwired (ruling R3)
// until Phase-24 lands statement ingestion → standing INCONCLUSIVE.
//
// SETTLEMENT legs (spec §5.19/§17.1): every SETTLED instruction must
// carry a non-VOID nostro_movements row with equal amount and the PAY→
// DEBIT / RECEIVE→CREDIT direction map; a POSTED movement on a
// non-SETTLED instruction is a movement without settlement authority →
// MISMATCH; overdue PENDING instructions are INCONCLUSIVE (ruling R6).
//
// FEES legs: per-trade expected fees (trades.buyer_fee in base /
// seller_fee in quote) vs collected wallet FEE ledger rows, and the GL
// 4010_TRADING_FEE_REVENUE_{ccy} credit per trade — collected ==
// expected AND revenue == collected.

import (
	"context"
	"strconv"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// FUNDING
// ---------------------------------------------------------------------------

// FundingChecker reconciles funding_transactions against the rail and
// suspense legs, plus the bank-statement seam.
type FundingChecker struct {
	Src        FundingSource
	Statements StatementSource // nil → standing INCONCLUSIVE (ruling R3)
}

func (FundingChecker) Name() Category { return CatFunding }

func (c FundingChecker) Run(ctx context.Context, s Scope) ([]Finding, error) {
	if c.Src == nil {
		return []Finding{InconclusiveFinding(CatFunding,
			string(CatFunding), "source", "no funding source wired")}, nil
	}
	var out []Finding

	withdrawals, err := c.Src.CompletedWithdrawals(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range withdrawals {
		subject := fundingSubject(w.FundingID)
		railScope := func() (string, string) {
			if w.BankMethod != "" {
				return "RAIL", w.BankMethod
			}
			return "GLOBAL", ""
		}
		switch {
		case w.RailPaymentID == nil:
			out = append(out, AmountFinding(CatFunding, subject,
				"funding_vs_rail", w.Amount, decimal.Zero, UnitAmount).
				WithDetail(map[string]any{
					"reason": "COMPLETED withdrawal has no rail_payments row",
					"rail":   w.BankMethod, "currency": w.Currency,
				}).WithHalt(railScope()))
		case w.RailStatus == "FAILED" || w.RailStatus == "REJECTED" ||
			w.RailStatus == "RETURNED":
			out = append(out, AmountFinding(CatFunding, subject,
				"funding_vs_rail", w.Amount, decimal.Zero, UnitAmount).
				WithDetail(map[string]any{
					"reason": "COMPLETED withdrawal's rail payment ended " + w.RailStatus,
					"rail":   w.BankMethod, "currency": w.Currency,
					"rail_payment_id": *w.RailPaymentID,
				}).WithHalt(railScope()))
		case w.RailStatus != "SETTLED":
			// PREPARED/DISPATCHED/ACKNOWLEDGED on a COMPLETED funding
			// row — the wallet moved, the rail is still in flight.
			out = append(out, InconclusiveFinding(CatFunding, subject,
				"rail_in_flight",
				"COMPLETED withdrawal's rail payment is "+w.RailStatus+
					" (in-flight — settles asynchronously)").
				WithDetail(map[string]any{
					"rail": w.BankMethod, "currency": w.Currency,
					"rail_payment_id": *w.RailPaymentID,
				}))
		}
	}

	quar, err := c.Src.QuarantinedDeposits(ctx)
	if err != nil {
		return nil, err
	}
	for _, q := range quar {
		out = append(out, AmountFinding(CatFunding,
			fundingSubject(q.FundingID),
			"suspense_vs_status", decimal.Zero, q.Amount, UnitAmount).
			WithDetail(map[string]any{
				"reason":            "deposit is " + q.Status + " while suspense mapping is " + q.QuarantineStatus,
				"mapping_id":        q.MappingID,
				"quarantine_status": q.QuarantineStatus,
				"funding_status":    q.Status,
			}).WithHalt("ACCOUNT", strconv.FormatInt(q.AccountID, 10)))
	}

	out = append(out, c.statementLeg(ctx, s)...)
	return out, nil
}

// statementLeg runs the bank-side diff when a StatementSource is wired;
// otherwise the standing inconclusive marker (ruling R3). An
// unattributed bank credit is INCONCLUSIVE (a statement line cannot be
// proven to be "ours" without the attribution chain).
func (c FundingChecker) statementLeg(ctx context.Context, s Scope) []Finding {
	if c.Statements == nil {
		return []Finding{InconclusiveFinding(CatFunding,
			string(CatFunding), "bank_statements",
			"no bank-statement source wired — external leg unverifiable "+
				"(statement ingestion lands with Phase-24)")}
	}
	lines, err := c.Statements.StatementLines(ctx, s.Now.Add(-24*time.Hour))
	if err != nil {
		return []Finding{InconclusiveFinding(CatFunding,
			string(CatFunding), "bank_statements", err.Error())}
	}
	var out []Finding
	for _, ln := range lines {
		subject := "statement:" + ln.BankTxID
		dep, err := c.Src.DepositForBankTx(ctx, ln.BankTxID)
		if err != nil {
			out = append(out, InconclusiveFinding(CatFunding, subject,
				"bank_statements", "match query failed: "+err.Error()))
			continue
		}
		if dep == nil {
			out = append(out, InconclusiveFinding(CatFunding, subject,
				"bank_statements",
				"unattributed bank credit — no funding row or suspense mapping").
				WithDetail(map[string]any{
					"currency": ln.Currency, "amount": ln.Amount.String(),
				}))
			continue
		}
		if !ln.Amount.Equal(dep.Amount) || ln.Currency != dep.Currency {
			out = append(out, AmountFinding(CatFunding, subject,
				"statement_vs_funding", dep.Amount, ln.Amount, UnitAmount).
				WithDetail(map[string]any{
					"funding_id": dep.FundingID, "statement_ccy": ln.Currency,
					"funding_ccy": dep.Currency,
				}).WithHalt("ACCOUNT", strconv.FormatInt(dep.AccountID, 10)))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// SETTLEMENT
// ---------------------------------------------------------------------------

// SettlementChecker diffs settlement_instructions against the
// nostro_movements intent ledger (migration 112). No surgical halt
// scope exists on the settlement axis → GLOBAL (ruling R4).
type SettlementChecker struct {
	Src SettlementSource
}

func (SettlementChecker) Name() Category { return CatSettlement }

func (c SettlementChecker) Run(ctx context.Context, s Scope) ([]Finding, error) {
	if c.Src == nil {
		return []Finding{InconclusiveFinding(CatSettlement,
			string(CatSettlement), "source", "no settlement source wired")}, nil
	}
	legs, err := c.Src.SettledLegs(ctx)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, lg := range legs {
		subject := settlementSubject(lg.InstructionID)
		switch {
		case lg.MovementID == nil:
			out = append(out, AmountFinding(CatSettlement, subject,
				"instruction_vs_movement", lg.Amount, decimal.Zero, UnitAmount).
				WithDetail(map[string]any{
					"reason":   "SETTLED instruction has no nostro_movements row",
					"currency": lg.Currency, "direction": lg.Direction,
				}).WithHalt("GLOBAL", ""))
		case lg.MovementStatus == "VOID":
			out = append(out, AmountFinding(CatSettlement, subject,
				"instruction_vs_movement", lg.Amount, decimal.Zero, UnitAmount).
				WithDetail(map[string]any{
					"reason":      "SETTLED instruction's movement is VOID",
					"movement_id": *lg.MovementID,
				}).WithHalt("GLOBAL", ""))
		case lg.MovementStatus == "PENDING":
			out = append(out, InconclusiveFinding(CatSettlement, subject,
				"movement_pending",
				"SETTLED instruction's movement still PENDING — nostro poster lag").
				WithDetail(map[string]any{"movement_id": *lg.MovementID}))
		default: // POSTED — verify amount + direction semantics
			wantDir := "DEBIT"
			if lg.Direction == "RECEIVE" {
				wantDir = "CREDIT"
			}
			if !lg.MovementAmount.Equal(lg.Amount) {
				out = append(out, AmountFinding(CatSettlement, subject,
					"instruction_vs_movement", lg.Amount, lg.MovementAmount,
					UnitAmount).
					WithDetail(map[string]any{
						"reason": "movement amount diverges", "currency": lg.Currency,
						"movement_id": *lg.MovementID,
					}).WithHalt("GLOBAL", ""))
			}
			if lg.MovementDir != wantDir {
				out = append(out, AmountFinding(CatSettlement, subject,
					"instruction_vs_movement", lg.Amount, lg.MovementAmount,
					UnitState).
					WithDetail(map[string]any{
						"reason":          "movement direction inverts instruction semantics",
						"instruction_dir": lg.Direction,
						"movement_dir":    lg.MovementDir,
						"movement_id":     *lg.MovementID,
					}).WithHalt("GLOBAL", ""))
			}
		}
	}

	orphans, err := c.Src.PostedOrphans(ctx)
	if err != nil {
		return nil, err
	}
	for _, o := range orphans {
		out = append(out, AmountFinding(CatSettlement,
			settlementSubject(o.InstructionID),
			"movement_vs_instruction", decimal.Zero, o.Amount, UnitAmount).
			WithDetail(map[string]any{
				"reason": "POSTED nostro movement on a " + o.InstructionStat +
					" instruction — applied without settlement authority",
				"movement_id": o.MovementID, "currency": o.Currency,
			}).WithHalt("GLOBAL", ""))
	}

	overdue, err := c.Src.OverduePending(ctx, s.Now)
	if err != nil {
		return nil, err
	}
	for _, id := range overdue {
		out = append(out, InconclusiveFinding(CatSettlement,
			settlementSubject(id), "overdue_pending",
			"PENDING instruction past its settlement_date — ops signal (ruling R6)"))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// FEES
// ---------------------------------------------------------------------------

// FeesChecker verifies collected == expected per trade and GL revenue ==
// collected. A fee divergence mischarges customers → ACCOUNT halt on the
// affected account.
type FeesChecker struct {
	Src FeeSource
}

func (FeesChecker) Name() Category { return CatFees }

func (c FeesChecker) Run(ctx context.Context, _ Scope) ([]Finding, error) {
	if c.Src == nil {
		return []Finding{InconclusiveFinding(CatFees,
			string(CatFees), "source", "no fee source wired")}, nil
	}
	expected, err := c.Src.ExpectedFees(ctx)
	if err != nil {
		return nil, err
	}
	collected, err := c.Src.CollectedFees(ctx)
	if err != nil {
		return nil, err
	}
	revenue, err := c.Src.FeeRevenue(ctx)
	if err != nil {
		return nil, err
	}

	type legKey struct {
		tradeID, accountID int64
		ccy                string
	}
	expLegs := map[legKey]decimal.Decimal{}
	for _, e := range expected {
		if !e.BuyerFee.IsZero() {
			expLegs[legKey{e.TradeID, e.BuyerID, e.BaseCurrency}] = e.BuyerFee
		}
		if !e.SellerFee.IsZero() {
			expLegs[legKey{e.TradeID, e.SellerID, e.QuoteCurrency}] = e.SellerFee
		}
	}
	colLegs := map[legKey]decimal.Decimal{}
	for _, f := range collected {
		colLegs[legKey{f.TradeID, f.AccountID, f.Currency}] = f.Outflow
	}

	var out []Finding
	haltFor := func(acct int64) (string, string) {
		return "ACCOUNT", strconv.FormatInt(acct, 10)
	}
	detailFor := func(k legKey, role string) map[string]any {
		return map[string]any{"trade_id": k.tradeID, "account_id": k.accountID,
			"currency": k.ccy, "leg": role}
	}
	for k, exp := range expLegs {
		col, ok := colLegs[k]
		if !ok {
			out = append(out, AmountFinding(CatFees, tradeSubject(k.tradeID),
				"expected_vs_collected", exp, decimal.Zero, UnitAmount).
				WithDetail(mergeDetail(detailFor(k, "expected"), "reason",
					"expected fee has no collected ledger row")).
				WithHalt(haltFor(k.accountID)))
			continue
		}
		if !col.Equal(exp) {
			out = append(out, AmountFinding(CatFees, tradeSubject(k.tradeID),
				"expected_vs_collected", exp, col, UnitAmount).
				WithDetail(mergeDetail(detailFor(k, "expected"), "reason",
					"collected fee diverges from trades.*_fee")).
				WithHalt(haltFor(k.accountID)))
		}
	}
	for k, col := range colLegs {
		if _, ok := expLegs[k]; ok {
			continue
		}
		if !col.IsZero() {
			out = append(out, AmountFinding(CatFees, tradeSubject(k.tradeID),
				"collected_vs_expected", decimal.Zero, col, UnitAmount).
				WithDetail(mergeDetail(detailFor(k, "collected"), "reason",
					"ledger FEE row with no expected trade fee")).
				WithHalt(haltFor(k.accountID)))
		}
	}

	// GL leg: revenue credited per (trade, ccy) equals the expected side.
	type tk struct {
		tradeID int64
		ccy     string
	}
	rev := map[tk]decimal.Decimal{}
	for _, r := range revenue {
		rev[tk{r.TradeID, r.Currency}] = r.Revenue
	}
	// expected revenue per (trade,ccy) = sum of that ccy's leg fees.
	expRev := map[tk]decimal.Decimal{}
	for k, exp := range expLegs {
		key := tk{k.tradeID, k.ccy}
		expRev[key] = expRev[key].Add(exp)
	}
	for k, exp := range expRev {
		got, ok := rev[k]
		if !ok {
			got = decimal.Zero
		}
		if !got.Equal(exp) {
			out = append(out, AmountFinding(CatFees,
				tradeSubject(k.tradeID)+":"+k.ccy,
				"fee_revenue_vs_expected", exp, got, UnitAmount).
				WithDetail(map[string]any{
					"reason":   "4010_TRADING_FEE_REVENUE credit diverges from expected fees",
					"currency": k.ccy,
				}).WithHalt("GLOBAL", ""))
		}
	}
	for k, got := range rev {
		if _, ok := expRev[k]; ok {
			continue
		}
		if !got.IsZero() {
			out = append(out, AmountFinding(CatFees,
				tradeSubject(k.tradeID)+":"+k.ccy,
				"fee_revenue_orphan", decimal.Zero, got, UnitAmount).
				WithDetail(map[string]any{
					"reason":   "fee revenue posted for a trade with no expected fee",
					"currency": k.ccy,
				}).WithHalt("GLOBAL", ""))
		}
	}
	return out, nil
}

func mergeDetail(d map[string]any, k string, v any) map[string]any {
	d[k] = v
	return d
}
