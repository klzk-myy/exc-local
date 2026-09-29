// compliance.go — Phase-18 Task 18.3.10 item 2/4: quoting-obligation
// tracking and the rolling-breach suspension rule.
//
// Sampling contract (spec §9.6): a registered MM must maintain
// two-sided quotes ≥ min_quote_size within max_spread_bps for ≥
// presence_pct of the trading day, sampled per minute. The FIX mass-
// quoting path (internal/fix/quoting.go) calls ObserveQuote after
// every applied quote set — each observation lands in today's
// mm_compliance row. RollupDay closes the day: presence_pct is
// computed, compared to the program floor, and a breach row is marked
// when short. BreachEvaluate then suspends the program once 3 daily
// failures land inside a rolling week (task text), pausing rebate
// accrual and alerting Risk Management.
package marketmaking

import (
	"context"
	"fmt"
	"time"

	"exchange/internal/observability"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ObligationVerdict is one obligation sample outcome: did the MM's
// current two-sided state satisfy the program's size + spread floors.
type ObligationVerdict struct {
	SizeOK   bool // both sides ≥ min_quote_size
	SpreadOK bool // (offer-bid)/mid ≤ max_spread_bps
}

// Compliant reports whether the sample meets the full obligation.
func (v ObligationVerdict) Compliant() bool { return v.SizeOK && v.SpreadOK }

// EvaluateObligation prices one two-sided quote against the program's
// obligation — pure logic for the quoting path and tests. A missing
// side fails size; spread is measured in basis points of mid.
func EvaluateObligation(p *Program, bidPx, bidQty, offerPx, offerQty *decimal.Decimal) ObligationVerdict {
	var v ObligationVerdict
	if bidPx == nil || offerPx == nil || bidQty == nil || offerQty == nil {
		return v
	}
	v.SizeOK = bidQty.GreaterThanOrEqual(p.MinQuoteSize) &&
		offerQty.GreaterThanOrEqual(p.MinQuoteSize)
	if bidPx.IsPositive() && offerPx.IsPositive() && offerPx.GreaterThan(*bidPx) {
		// spread_bps = (offer - bid) / ((offer + bid) / 2) * 10000
		mid := bidPx.Add(*offerPx).Div(decimal.NewFromInt(2))
		spreadBps := offerPx.Sub(*bidPx).Div(mid).Mul(decimal.NewFromInt(10_000))
		v.SpreadOK = spreadBps.LessThanOrEqual(p.MaxSpreadBps)
	}
	return v
}

// ObserveQuote records one obligation sample for every ACTIVE program
// covering (accountID, instrumentID) — the program-wide row AND the
// per-instrument row each carry their own obligations, so both are
// scored. Programs whose status is not ACTIVE are skipped (a suspended
// program cannot cure or worsen its compliance day).
func (s *Service) ObserveQuote(ctx context.Context, accountID, instrumentID int64,
	bidPx, bidQty, offerPx, offerQty *decimal.Decimal) error {
	rows, err := s.store.ProgramsFor(ctx, accountID, instrumentID)
	if err != nil {
		return excerrors.Wrap(CodeMMInternal, "mm program lookup", err)
	}
	day := s.todayUTC()
	for i := range rows {
		p := &rows[i]
		if p.Status != StatusActive {
			continue
		}
		v := EvaluateObligation(p, bidPx, bidQty, offerPx, offerQty)
		if err := s.store.RecordSample(ctx, p.ID, day, v.Compliant()); err != nil {
			return excerrors.Wrap(CodeMMInternal, "mm compliance sample", err)
		}
	}
	return nil
}

func (s *Service) todayUTC() time.Time {
	n := s.now().UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

// RollupDay closes (or re-scores) one program's compliance day:
// presence_pct = compliant/total·100; breach when no samples exist or
// the computed presence is under the program floor. A day with zero
// samples breaches — presence obligations are measured over the trading
// day and silence is non-compliance (fail-closed, spec §2.7); the
// 24/5 weekend boundary is calendar-owned by the caller, which simply
// does not rollup closed days.
func (s *Service) RollupDay(ctx context.Context, programID int64, day time.Time) (*ComplianceRow, error) {
	p, err := s.Get(ctx, programID)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ComplianceRows(ctx, programID, day, day)
	if err != nil {
		return nil, excerrors.Wrap(CodeMMInternal, "mm compliance read", err)
	}
	var row ComplianceRow
	if len(rows) > 0 {
		row = rows[0]
	} else {
		row = ComplianceRow{ProgramID: programID, Day: day}
	}
	presence := decimal.Zero
	if row.SamplesTotal > 0 {
		presence = decimal.NewFromInt(int64(row.SamplesCompliant)).
			Mul(decimal.NewFromInt(100)).
			Div(decimal.NewFromInt(int64(row.SamplesTotal)))
	}
	breach := presence.LessThan(p.PresencePct)
	reason := ""
	if breach {
		reason = fmt.Sprintf("PRESENCE_SHORTFALL presence=%s%% floor=%s%% samples=%d",
			presence.StringFixed(2), p.PresencePct.StringFixed(2), row.SamplesTotal)
	}
	if err := s.store.FinalizeCompliance(ctx, programID, day, presence, breach, reason); err != nil {
		return nil, excerrors.Wrap(CodeMMInternal, "mm compliance finalize", err)
	}
	row.PresencePct = &presence
	row.Breach = breach
	row.BreachReason = reason
	if breach {
		s.raise(ctx, observability.Alert{
			Rule:     "mm_obligation_breach",
			Severity: observability.SeverityP2,
			Code:     CodeMMObligationBreach,
			Summary: fmt.Sprintf("MM program %d daily obligation breach: %s",
				programID, reason),
			Status: "firing",
			Details: map[string]string{
				"program_id":   fmt.Sprint(programID),
				"account_id":   fmt.Sprint(p.AccountID),
				"day":          day.Format("2006-01-02"),
				"presence_pct": presence.String(),
			},
			FiredAt: s.now().UTC().Format(time.RFC3339Nano),
		})
		if err := s.evaluateSuspension(ctx, p); err != nil {
			return &row, err
		}
	}
	return &row, nil
}

// evaluateSuspension implements the task's rolling suspension rule:
// ≥3 daily breaches inside BreachSuspendWindow suspend the program.
func (s *Service) evaluateSuspension(ctx context.Context, p *Program) error {
	n, err := s.store.BreachCount(ctx, p.ID, s.now().UTC().Add(-BreachSuspendWindow))
	if err != nil {
		return excerrors.Wrap(CodeMMInternal, "mm breach count", err)
	}
	if n < BreachSuspendThreshold {
		return nil
	}
	return s.Suspend(ctx, p.ID,
		fmt.Sprintf("%d daily compliance breaches in rolling %ds window", n, int(BreachSuspendWindow.Seconds())))
}

// Compliance returns the program's daily rollup rows for the range.
func (s *Service) Compliance(ctx context.Context, programID int64, from, to time.Time) ([]ComplianceRow, error) {
	rows, err := s.store.ComplianceRows(ctx, programID, from, to)
	if err != nil {
		return nil, excerrors.Wrap(CodeMMInternal, "mm compliance list", err)
	}
	return rows, nil
}
