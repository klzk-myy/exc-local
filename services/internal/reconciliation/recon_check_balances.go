package reconciliation

// check_balances.go — BALANCES (category 1) and GENERAL_LEDGER
// (category 9).

import (
	"context"
	"strconv"

	"exchange/pkg/decimal"
)

// BalancesChecker reconciles balances.total against the append-only
// wallet ledger — the Task-4.3.9 reconcile's own scan legs composed
// through BalancesSource (never a second implementation).
//
// Halt mapping: a money divergence has an account boundary → ACCOUNT
// halt. "journal_sums_drift" (ledger==live but the verification cache
// disagrees) is INCONCLUSIVE — the money is provably right; the cache
// needs repair, not a halt (ruling R1).
type BalancesChecker struct {
	Src BalancesSource
}

func (BalancesChecker) Name() Category { return CatBalances }

func (c BalancesChecker) Run(ctx context.Context, _ Scope) ([]Finding, error) {
	if c.Src == nil {
		return []Finding{InconclusiveFinding(CatBalances,
			string(CatBalances), "source", "no balances source wired")}, nil
	}
	res, err := c.Src.WalletDiff(ctx)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, m := range res.Mismatches {
		subject := accountSubject(m.AccountID, m.Currency)
		detail := map[string]any{
			"kind":            m.Kind,
			"entry_count":     m.EntryCount,
			"last_entry_id":   m.LastEntryID,
			"journal_sums":    m.JournalSumsNet,
			"wallets_checked": res.WalletsChecked,
		}
		if m.Kind == "journal_sums_drift" {
			out = append(out, InconclusiveFinding(CatBalances, subject,
				"journal_sums",
				"journal_sums.net_balance drifted from ledger=live — cache repair, money verified").
				WithDetail(detail))
			continue
		}
		expected := decOrZero(m.LedgerNet)
		actual := decOrZero(m.LiveTotal)
		f := AmountFinding(CatBalances, subject, "ledger_vs_balances",
			expected, actual, UnitAmount).
			WithDetail(detail).
			WithHalt("ACCOUNT", strconv.FormatInt(m.AccountID, 10))
		out = append(out, f)
	}
	return out, nil
}

func decOrZero(s string) decimal.Decimal {
	if s == "" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return d
}

// GeneralLedgerChecker verifies the §5.21/§5.40 zero-sum invariant in
// two independent legs: per (journal_entry, currency) — what the
// deferred gl_journal_zero_sum_chk trigger enforces at commit — and
// globally per currency across all ledger_lines. Rows that escaped the
// trigger (hot-patched rows, deferred-trigger disablement, replica
// divergence) surface here; a divergence has no surgical boundary →
// GLOBAL halt.
type GeneralLedgerChecker struct {
	Src GLSource
}

func (GeneralLedgerChecker) Name() Category { return CatGeneralLedger }

func (c GeneralLedgerChecker) Run(ctx context.Context, _ Scope) ([]Finding, error) {
	if c.Src == nil {
		return []Finding{InconclusiveFinding(CatGeneralLedger,
			string(CatGeneralLedger), "source", "no GL source wired")}, nil
	}
	sums, err := c.Src.CurrencySums(ctx)
	if err != nil {
		return nil, err
	}
	imbs, err := c.Src.JournalImbalances(ctx)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, s := range sums {
		if !s.Debits.Equal(s.Credits) {
			out = append(out, AmountFinding(CatGeneralLedger,
				currencySubject(s.Currency), "ledger_lines_zero_sum",
				s.Debits, s.Credits, UnitAmount).
				WithHalt("GLOBAL", ""))
		}
	}
	for _, j := range imbs {
		out = append(out, AmountFinding(CatGeneralLedger,
			journalSubject(j.JournalEntryID, j.Currency),
			"journal_zero_sum",
			j.Debits, j.Credits, UnitAmount).
			WithHalt("GLOBAL", ""))
	}
	return out, nil
}
