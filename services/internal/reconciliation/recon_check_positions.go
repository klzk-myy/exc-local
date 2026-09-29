package reconciliation

// check_positions.go — POSITIONS (category 2) and PNL (category 8).
//
// POSITIONS: no AdminGetPositions query seam exists against the C++
// core (ruling R2), so the checkable legs are the PostgreSQL positions
// projection vs the position_fills system-of-record (migration 111 —
// the idempotent fill-application log). The missing core leg is a
// standing INCONCLUSIVE marker, never a fabricated pass.
//
// PNL: positions.realized_pnl is verified against SUM(position_fills
// .realized_pnl); unrealized_pnl against (mark − entry) × signed qty.
// The "vs balance changes" side is verified transitively: BALANCES
// proves the ledger equals the wallet and every P&L-bearing cashflow
// posts through the same ledger (ruling documented in doc.go).

import (
	"context"
	"strconv"

	"exchange/pkg/decimal"
)

// PositionsChecker diffs PG positions vs the fill-ledger projection.
type PositionsChecker struct {
	Src PositionSource
}

func (PositionsChecker) Name() Category { return CatPositions }

func (c PositionsChecker) Run(ctx context.Context, _ Scope) ([]Finding, error) {
	if c.Src == nil {
		return []Finding{InconclusiveFinding(CatPositions,
			string(CatPositions), "source", "no position source wired")}, nil
	}
	positions, err := c.Src.Positions(ctx)
	if err != nil {
		return nil, err
	}
	fills, err := c.Src.FillNets(ctx)
	if err != nil {
		return nil, err
	}

	nets := map[[2]int64]decimal.Decimal{}
	for _, f := range fills {
		nets[[2]int64{f.AccountID, f.InstrumentID}] = f.NetQty
	}

	var out []Finding
	// Standing marker: the C++ core leg has no query seam — recorded so
	// the report never implies a two-sided verification that cannot run.
	out = append(out, InconclusiveFinding(CatPositions,
		string(CatPositions), "core_state",
		"no AdminGetPositions seam against the C++ core — PG↔projection leg only"))

	seen := map[[2]int64]bool{}
	for _, p := range positions {
		key := [2]int64{p.AccountID, p.InstrumentID}
		seen[key] = true
		net, ok := nets[key]
		expected := net
		if !ok {
			expected = decimal.Zero
		}
		if !p.SignedQty().Round(8).Equal(expected.Round(8)) {
			out = append(out, AmountFinding(CatPositions,
				positionSubject(p.AccountID, p.InstrumentID),
				"pg_vs_fill_projection",
				expected, p.SignedQty(), UnitQty).
				WithDetail(map[string]any{
					"side":     p.Side,
					"quantity": p.Quantity.String(),
				}).WithHalt("ACCOUNT", strconv.FormatInt(p.AccountID, 10)))
		}
	}
	for _, f := range fills {
		key := [2]int64{f.AccountID, f.InstrumentID}
		if seen[key] {
			continue
		}
		if !f.NetQty.IsZero() {
			out = append(out, AmountFinding(CatPositions,
				positionSubject(f.AccountID, f.InstrumentID),
				"fills_vs_pg",
				f.NetQty, decimal.Zero, UnitQty).
				WithDetail(map[string]any{
					"reason": "position_fills nets a non-zero position with no positions row",
				}).WithHalt("ACCOUNT", strconv.FormatInt(f.AccountID, 10)))
		}
	}
	return out, nil
}

// PnLChecker verifies realized + unrealized P&L bookkeeping per
// (account, instrument). A P&L divergence misreports customer equity →
// the halt resolves to the affected account.
type PnLChecker struct {
	Src PositionSource
}

func (PnLChecker) Name() Category { return CatPnL }

func (c PnLChecker) Run(ctx context.Context, _ Scope) ([]Finding, error) {
	if c.Src == nil {
		return []Finding{InconclusiveFinding(CatPnL,
			string(CatPnL), "source", "no position source wired")}, nil
	}
	positions, err := c.Src.Positions(ctx)
	if err != nil {
		return nil, err
	}
	fills, err := c.Src.FillNets(ctx)
	if err != nil {
		return nil, err
	}
	realized := map[[2]int64]decimal.Decimal{}
	for _, f := range fills {
		realized[[2]int64{f.AccountID, f.InstrumentID}] = f.RealizedPnL
	}

	var out []Finding
	halt := func(acct int64) (string, string) {
		return "ACCOUNT", strconv.FormatInt(acct, 10)
	}
	for _, p := range positions {
		key := [2]int64{p.AccountID, p.InstrumentID}
		subject := positionSubject(p.AccountID, p.InstrumentID)
		sc, tg := halt(p.AccountID)

		// Leg 1: realized P&L vs the fill ledger.
		exp, ok := realized[key]
		if !ok {
			exp = decimal.Zero
		}
		if !exp.Equal(p.RealizedPnL) {
			out = append(out, AmountFinding(CatPnL, subject,
				"realized_vs_fills", exp, p.RealizedPnL, UnitAmount).
				WithDetail(map[string]any{"currency_basis": "quote"}).
				WithHalt(sc, tg))
		}

		// Leg 2: unrealized P&L vs (mark − entry) × signed qty. A nil
		// mark makes the leg unverifiable, not zero — INCONCLUSIVE.
		if p.MarkPrice == nil {
			if !p.UnrealizedPnL.IsZero() || !p.Quantity.IsZero() {
				out = append(out, InconclusiveFinding(CatPnL, subject,
					"unrealized",
					"mark_price absent — unrealized P&L unverifiable"))
			}
			continue
		}
		expectedUnreal := p.MarkPrice.Sub(p.EntryPrice).Mul(p.SignedQty())
		if !expectedUnreal.Round(8).Equal(p.UnrealizedPnL.Round(8)) {
			out = append(out, AmountFinding(CatPnL, subject,
				"unrealized_vs_mark",
				expectedUnreal, p.UnrealizedPnL, UnitAmount).
				WithDetail(map[string]any{
					"mark_price":  p.MarkPrice.String(),
					"entry_price": p.EntryPrice.String(),
					"signed_qty":  p.SignedQty().String(),
				}).WithHalt(sc, tg))
		}
	}
	// Fill-ledger realized sums with no positions row — the POSITIONS
	// checker already flags the missing row; the P&L side flags the
	// unbooked realized amount itself.
	seen := map[[2]int64]bool{}
	for _, p := range positions {
		seen[[2]int64{p.AccountID, p.InstrumentID}] = true
	}
	for _, f := range fills {
		if seen[[2]int64{f.AccountID, f.InstrumentID}] {
			continue
		}
		if !f.RealizedPnL.IsZero() {
			out = append(out, AmountFinding(CatPnL,
				positionSubject(f.AccountID, f.InstrumentID),
				"fills_vs_pg", f.RealizedPnL, decimal.Zero, UnitAmount).
				WithDetail(map[string]any{
					"reason": "realized P&L in fill ledger with no positions row",
				}).WithHalt("ACCOUNT", strconv.FormatInt(f.AccountID, 10)))
		}
	}
	return out, nil
}
