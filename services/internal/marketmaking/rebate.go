// rebate.go — Phase-18 Task 18.3.10 item 5: maker rebates.
//
// Per spec §9.6 each maker fill on an instrument covered by an ACTIVE
// program accrues rebate_bps of the fill's quote-currency notional.
// Accruals land in mm_rebate_accruals (one row per fill, idempotent on
// fill_ref); the monthly posting sweeps unposted rows into one balanced
// journal per (account, currency): DR 5100_LIQUIDITY_REBATE_EXPENSE /
// CR 2010_CUSTOMER_LIABILITY plus a wallet credit — the same shape as
// settlement.BuildFeeJournal's rebate branch, never a direct balance
// write (spec §5.3 zero GL-bypass).
//
// Accrual pauses during suspension (task text item 4) and for
// rebate_bps = 0 programs.
package marketmaking

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// RebatePoster binds the GL poster (nil → accrual-only service;
// PostAccruedRebates refuses).
func (s *Service) WithRebatePoster(p JournalPoster) *Service {
	s.poster = p
	return s
}

// AccrueRebate records one maker fill's rebate for the covering ACTIVE
// program. fillRef is the caller's idempotency token (engine fill seq
// preferred; the gateway read-model hook derives "order:{id}:{px}:{qty}"
// when no fill id is available) — a replayed ref is a no-op. Suspended
// programs accrue nothing (accrual pause), matching the task text.
func (s *Service) AccrueRebate(ctx context.Context, accountID, instrumentID int64,
	fillRef string, qty, price decimal.Decimal) (decimal.Decimal, error) {
	p, err := s.Entitled(ctx, accountID, instrumentID)
	if err != nil {
		return decimal.Zero, err
	}
	if p == nil || p.RebateBps.IsZero() {
		return decimal.Zero, nil // no program / no rebate tier
	}
	if !qty.IsPositive() || !price.IsPositive() {
		return decimal.Zero, excerrors.New(CodeMMInvalid,
			"rebate accrual requires positive qty and price")
	}
	sym, err := s.store.InstrumentSymbol(ctx, instrumentID)
	if err != nil {
		return decimal.Zero, excerrors.Wrap(CodeMMInternal, "instrument lookup", err)
	}
	ccy, err := s.quoteCurrency(ctx, instrumentID)
	if err != nil {
		return decimal.Zero, err
	}
	// amount = qty × price × rebate_bps / 10_000, 8dp quantum.
	amount := qty.Mul(price).Mul(p.RebateBps).
		Div(decimal.NewFromInt(10_000)).Round(8)
	if !amount.IsPositive() {
		return decimal.Zero, nil // sub-quantum accrual — nothing material
	}
	if strings.TrimSpace(fillRef) == "" {
		fillRef = fmt.Sprintf("order:%d:%s:%s", instrumentID, price.String(), qty.String())
	}
	if len(fillRef) > 64 {
		fillRef = fillRef[:64]
	}
	err = s.store.AccrueRebate(ctx, RebateAccrual{
		ProgramID:    p.ID,
		AccountID:    accountID,
		InstrumentID: instrumentID,
		FillRef:      fillRef,
		Day:          s.todayUTC(),
		Currency:     ccy,
		Amount:       amount,
	})
	if err != nil {
		return decimal.Zero, excerrors.Wrap(CodeMMInternal,
			fmt.Sprintf("mm rebate accrual (%s)", sym), err)
	}
	return amount, nil
}

func (s *Service) quoteCurrency(ctx context.Context, instrumentID int64) (string, error) {
	ccy, err := s.store.QuoteCurrency(ctx, instrumentID)
	if err != nil {
		return "", excerrors.Wrap(CodeMMInternal, "instrument quote ccy", err)
	}
	if len(ccy) != 3 {
		return "", excerrors.New(CodeMMInternal,
			fmt.Sprintf("instrument %d quote currency %q invalid", instrumentID, ccy))
	}
	return ccy, nil
}

// rebateGroup accumulates unposted accruals per (account, currency).
type rebateGroup struct {
	accountID int64
	currency  string
	amount    decimal.Decimal
	ids       []int64
}

// BuildRebateJournal renders the monthly GL journal for one
// (account, currency) accrual group — DR liquidity-rebate expense /
// CR customer liability, wallet credit effect. Idempotency key binds
// the covered accrual id set so a replayed sweep resolves to the
// committed journal rather than double-paying.
func BuildRebateJournal(g rebateGroup, postedBy string) (ledger.Journal, error) {
	if g.accountID <= 0 || len(g.ids) == 0 {
		return ledger.Journal{}, excerrors.New(CodeMMInvalid,
			"rebate journal: empty accrual group")
	}
	if !g.amount.IsPositive() {
		return ledger.Journal{}, excerrors.New(CodeMMInvalid,
			"rebate journal: non-positive accrual amount")
	}
	sort.Slice(g.ids, func(a, b int) bool { return g.ids[a] < g.ids[b] })
	narrative := fmt.Sprintf("MM_REBATE acct=%d %s fills=%d amount=%s",
		g.accountID, g.currency, len(g.ids), g.amount.String())
	return ledger.Journal{
		EntryType:   ledger.EntryFee,
		Description: narrative,
		PostedBy:    postedBy,
		IdempotencyKey: fmt.Sprintf("mm-rebate:%d:%s:%d-%d",
			g.accountID, g.currency, g.ids[0], g.ids[len(g.ids)-1]),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.LiquidityRebateExpense(g.currency), g.currency, g.amount, narrative),
			ledger.CreditLine(ledger.CustomerLiability(g.currency), g.currency, g.amount, narrative),
		},
		Effects: []ledger.AccountEffect{{
			AccountID:      g.accountID,
			Currency:       g.currency,
			AvailableDelta: g.amount, // credit the MM's wallet
		}},
	}, nil
}

// PostAccruedRebates sweeps every unposted accrual row into one journal
// per (account, currency), posts each through the ledger path, then
// stamps posted_journal_id. Per-group failures leave the group's rows
// unposted for the next sweep — one bad currency never blocks the rest.
// Returns per-group journal ids keyed "accountID:currency".
func (s *Service) PostAccruedRebates(ctx context.Context) (map[string]int64, error) {
	if s.poster == nil {
		return nil, excerrors.New(CodeMMInternal,
			"mm rebate poster not wired — accrual-only service")
	}
	rows, err := s.store.UnpostedRebates(ctx)
	if err != nil {
		return nil, excerrors.Wrap(CodeMMInternal, "mm rebate sweep read", err)
	}
	groups := map[string]*rebateGroup{}
	for _, r := range rows {
		k := fmt.Sprintf("%d:%s", r.AccountID, r.Currency)
		g := groups[k]
		if g == nil {
			g = &rebateGroup{accountID: r.AccountID, currency: r.Currency}
			groups[k] = g
		}
		g.amount = g.amount.Add(r.Amount)
		g.ids = append(g.ids, r.ID)
	}
	posted := map[string]int64{}
	var firstErr error
	for k, g := range groups {
		j, err := BuildRebateJournal(*g, "marketmaking-service")
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		res, err := s.poster.Post(ctx, j)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("mm rebate post %s: %w", k, err)
			}
			continue
		}
		if err := s.store.MarkRebatesPosted(ctx, g.ids, res.JournalID); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("mm rebate mark-posted %s: %w", k, err)
			}
			continue
		}
		posted[k] = res.JournalID
	}
	return posted, firstErr
}

// Rebates lists a program's accrual rows (admin read path).
func (s *Service) Rebates(ctx context.Context, programID int64, limit int) ([]RebateAccrual, error) {
	rows, err := s.store.ListRebates(ctx, programID, limit)
	if err != nil {
		return nil, excerrors.Wrap(CodeMMInternal, "mm rebate list", err)
	}
	return rows, nil
}
