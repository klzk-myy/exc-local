package copy

import (
	"context"
	"fmt"
	"time"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
)

// JournalPoster is the §5.3 posting seam — settlement's
// DoubleEntryLedgerService satisfies it; nil fails closed.
type JournalPoster interface {
	Post(ctx context.Context, j ledger.Journal) (ledger.PostResult, error)
}

// SubledgerWriter appends PAMM-taxonomy rows for accruals — the shared
// pamm_subledger_entries table with copy_follow_id set (pool_id NULL).
// *pamm.PgxStore satisfies it via a thin adapter (see NewSubledgerWriter).
type SubledgerWriter interface {
	InsertSubledger(ctx context.Context, entries []SubledgerRow) error
}

// SubledgerRow is the copy-side projection of a pamm_subledger_entries
// row (kept package-local so the copy layer never reaches into pamm's
// internals for the shared table).
type SubledgerRow struct {
	TxnType        string // PAMM_FEE_PERF | PAMM_FEE_MGMT
	CopyFollowID   int64
	AccountID      int64
	Currency       string
	Direction      string // DEBIT | CREDIT
	Amount         decimal.Decimal
	JournalEntryID *int64
	ReferenceID    int64 // accrual_id
	Narrative      string
}

// SettleResult reports one profit-share settlement.
type SettleResult struct {
	FollowID        int64           `json:"follow_id"`
	PnL             decimal.Decimal `json:"pnl"` // computed period P&L (strategy ccy)
	WatermarkBefore decimal.Decimal `json:"watermark_before"`
	WatermarkAfter  decimal.Decimal `json:"watermark_after"` // ratchets iff accrued > 0
	Accrued         decimal.Decimal `json:"accrued"`         // profit share charged
	AccrualID       int64           `json:"accrual_id"`
	JournalID       int64           `json:"journal_id,omitempty"`
	Balanced        bool            `json:"balanced"` // GL zero-sum held
}

// SettleProfitShare computes accrual = max(0, pnl − watermark) × pct/100
// for the follow over [start,end] and posts it.
//
// Rules (spec §12.9, Task 14.3.14 step 3):
//   - P&L is CALLER-COMPUTED from the follow's copied fills and passed in —
//     the engine's PnLSource seam owns derivation; this function only
//     enforces the accounting;
//   - accrual only above the HWM: pnl ≤ watermark ⇒ accrued = 0 AND the
//     watermark HOLDS (loss months never reset the mark);
//   - payout posts a balanced TRANSFER journal: investor liability
//     (2010) → manager liability (2010) — the "investor P&L → manager
//     revenue" movement, plus the PAMM_FEE_PERF sub-ledger pair;
//   - the watermark ratchets to pnl only when accrued > 0;
//   - UNIQUE (follow_id, period_end) makes re-settlement idempotent —
//     a repeated period returns the stored accrual, never double-pays.
func (s *Service) SettleProfitShare(ctx context.Context, followID int64,
	periodStart, periodEnd time.Time, pnl decimal.Decimal) (*SettleResult, error) {
	if followID <= 0 {
		return nil, errorf(CodeInvalidRequest, "follow_id required")
	}
	if !periodEnd.After(periodStart) {
		return nil, errorf(CodeInvalidRequest, "period_end must follow period_start")
	}
	if s.poster == nil || s.subledger == nil {
		return nil, errorf(CodeServiceDegraded,
			"profit-share posting seams unbound — settlement refuses to run")
	}
	f, err := s.store.FollowByID(ctx, followID)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, errorf(CodeNotFound, "follow %d not found", followID)
	}
	st, err := s.store.StrategyByID(ctx, f.StrategyID)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, errorf(CodeNotFound, "strategy %d not found", f.StrategyID)
	}
	hwm, err := s.store.HWMForUpdate(ctx, followID)
	if err != nil {
		return nil, err
	}

	wmBefore := hwm.WatermarkPnL
	above := pnl.Sub(wmBefore)
	accrued := decimal.Zero
	if above.IsPositive() && st.ProfitSharePct.IsPositive() {
		accrued = above.Mul(st.ProfitSharePct).
			Div(decimal.NewFromInt(100)).Truncate(8) // floor at the quantum —
		// a manager can never be credited more than pct of the excess
	}
	wmAfter := wmBefore
	if accrued.IsPositive() && pnl.GreaterThan(wmBefore) {
		wmAfter = pnl
	}
	res := &SettleResult{FollowID: followID, PnL: pnl,
		WatermarkBefore: wmBefore, WatermarkAfter: wmAfter, Accrued: accrued}

	if accrued.IsPositive() {
		if err := s.checker.AssertMutable(ctx, f.InvestorAccountID); err != nil {
			return nil, err
		}
		if err := s.checker.AssertMutable(ctx, st.ManagerAccountID); err != nil {
			return nil, err
		}
		idem := fmt.Sprintf("copy:profit-share:%d:%s", followID,
			periodEnd.UTC().Format("20060102"))
		jres, perr := s.poster.Post(ctx, ledger.Journal{
			EntryType:      ledger.EntryTransfer,
			Description:    fmt.Sprintf("PAMM_FEE_PERF: strategy %d profit share %s %s follow %d", st.StrategyID, accrued, f.Currency, followID),
			PostedBy:       "copy-profit-share",
			IdempotencyKey: idem,
			Lines: []ledger.Line{
				ledger.DebitLine(ledger.CustomerLiability(f.Currency), f.Currency, accrued,
					fmt.Sprintf("follow %d investor performance fee to manager %d", followID, st.ManagerAccountID)),
				ledger.CreditLine(ledger.CustomerLiability(f.Currency), f.Currency, accrued,
					fmt.Sprintf("manager %d profit share follow %d", st.ManagerAccountID, followID)),
			},
			Effects: []ledger.AccountEffect{
				{AccountID: f.InvestorAccountID, Currency: f.Currency, AvailableDelta: accrued.Neg()},
				{AccountID: st.ManagerAccountID, Currency: f.Currency, AvailableDelta: accrued},
			},
		})
		if perr != nil && !jres.Committed {
			return nil, perr
		}
		jid := jres.JournalID
		res.JournalID = jid
		res.Balanced = true

		acc := &Accrual{FollowID: followID, StrategyID: st.StrategyID,
			PeriodStart: periodStart, PeriodEnd: periodEnd, PnL: pnl,
			Currency: f.Currency, WatermarkBefore: wmBefore,
			WatermarkAfter: wmAfter, AccruedAmount: accrued, JournalEntryID: &jid}
		ins, err := s.store.InsertAccrual(ctx, acc)
		if err != nil {
			return nil, err
		}
		res.AccrualID = ins.AccrualID

		// Sub-ledger taxonomy pair on the SHARED internal investment log —
		// copy_follow_id set, pool_id NULL. The investor leg CREDITs out of
		// the follow's scope; the manager leg DEBITs the revenue in.
		if err := s.subledger.InsertSubledger(ctx, []SubledgerRow{
			{TxnType: "PAMM_FEE_PERF", CopyFollowID: followID,
				AccountID: f.InvestorAccountID, Currency: f.Currency,
				Direction: "CREDIT", Amount: accrued, JournalEntryID: &jid,
				ReferenceID: ins.AccrualID,
				Narrative:   "performance fee out — follow " + fmt.Sprint(followID)},
			{TxnType: "PAMM_FEE_PERF", CopyFollowID: followID,
				AccountID: st.ManagerAccountID, Currency: f.Currency,
				Direction: "DEBIT", Amount: accrued, JournalEntryID: &jid,
				ReferenceID: ins.AccrualID,
				Narrative:   "performance fee in — manager " + fmt.Sprint(st.ManagerAccountID)},
		}); err != nil {
			return nil, err
		}
		if err := s.store.RatchetHWM(ctx, followID, wmAfter, periodEnd); err != nil {
			return nil, err
		}
		return res, nil
	}

	// Loss/below-mark period: record the zero accrual (audit trail that
	// settlement ran), HWM untouched — ratchet predicate is a no-op.
	acc := &Accrual{FollowID: followID, StrategyID: st.StrategyID,
		PeriodStart: periodStart, PeriodEnd: periodEnd, PnL: pnl,
		Currency: f.Currency, WatermarkBefore: wmBefore,
		WatermarkAfter: wmBefore, AccruedAmount: decimal.Zero}
	ins, err := s.store.InsertAccrual(ctx, acc)
	if err != nil {
		return nil, err
	}
	res.AccrualID = ins.AccrualID
	return res, nil
}
