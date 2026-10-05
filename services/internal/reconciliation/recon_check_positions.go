package reconciliation

// check_positions.go — POSITIONS (category 2) and PNL (category 8).
//
// POSITIONS legs: (a) the PostgreSQL positions projection vs the
// position_fills system-of-record (migration 111 — the idempotent
// fill-application log); (b) the engine-journal leg — the core's WAL
// ORDER_NEW (account_id) + TRADE entries replayed into per-(account,
// instrument) nets and diffed against the same projection (Task
// 10.5.3.26; this is the "core state" seam ruling R2 lacked — read-only
// sealed-segment replay, never an in-path query). An unwired source or
// incomplete journal coverage degrades to a targeted INCONCLUSIVE,
// never a fabricated pass.
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

// PositionsChecker diffs PG positions vs the fill-ledger projection,
// and — when Wal is wired — vs the engine-journal position nets.
type PositionsChecker struct {
	Src PositionSource
	// Wal is the engine-journal seam (replayWalSource in production).
	// nil → the core leg reports the standing INCONCLUSIVE marker.
	Wal walSource
}

func (PositionsChecker) Name() Category { return CatPositions }

func (c PositionsChecker) Run(ctx context.Context, s Scope) ([]Finding, error) {
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
	// Engine-journal leg (Task 10.5.3.26): WAL-derived position nets vs
	// the PG projection. An unwired seam keeps the standing marker; a
	// wired-but-incomplete journal degrades per walReplayOrInconclusive.
	if c.Wal == nil {
		out = append(out, InconclusiveFinding(CatPositions,
			string(CatPositions), "core_state",
			"no engine WAL replay seam wired — PG↔projection leg only"))
	} else {
		out = append(out, c.coreLeg(ctx, s, positions)...)
	}

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

// coreLeg diffs engine-journal position nets (WAL ORDER_NEW owners +
// TRADE fills) against the PG positions projection. TRADE legs whose
// order owner is not journaled and test-scoped accounts are excluded —
// the former degrades the whole leg to INCONCLUSIVE (the diff would
// under-count silently), the latter mirrors the PG read's own
// testScopedExclude filter.
func (c PositionsChecker) coreLeg(ctx context.Context, s Scope,
	positions []PgPosition) []Finding {
	replay, prob := walReplayOrInconclusive(ctx, s, c.Wal, CatPositions)
	if prob != nil {
		return []Finding{*prob}
	}
	nets, unresolved := replay.PositionNets()
	if unresolved > 0 {
		return []Finding{InconclusiveFinding(CatPositions,
			string(CatPositions), "core_state",
			"journaled trades reference orders absent from the journal — "+
				"engine-journal position nets incomplete").
			WithDetail(map[string]any{"unresolved_trades": unresolved})}
	}
	excluded, err := c.Src.ExcludedAccounts(ctx)
	if err != nil {
		return []Finding{InconclusiveFinding(CatPositions,
			string(CatPositions), "core_state",
			"test-scope exclusion set unreadable: "+err.Error())}
	}
	pgNets := map[[2]uint64]decimal.Decimal{}
	for _, p := range positions {
		pgNets[[2]uint64{uint64(p.AccountID), uint64(p.InstrumentID)}] = p.SignedQty()
	}
	var out []Finding
	for key, netUnits := range nets {
		if _, skip := excluded[int64(key[0])]; skip {
			continue
		}
		expected := decimal.NewFromScaled(netUnits)
		actual, ok := pgNets[key]
		if !ok {
			actual = decimal.Zero
		}
		if !expected.Round(8).Equal(actual.Round(8)) {
			out = append(out, AmountFinding(CatPositions,
				positionSubject(int64(key[0]), int64(key[1])),
				"wal_vs_pg", expected, actual, UnitQty).
				WithDetail(map[string]any{
					"reason": "engine journal nets a position the projection disagrees with",
				}).WithHalt("ACCOUNT", strconv.FormatInt(int64(key[0]), 10)))
		}
	}
	// Symmetric leg: a PG position the journal never netted means the
	// projection holds a row the engine never produced (or the journal
	// silently netted to zero — same verdict either way).
	for key, signed := range pgNets {
		if _, skip := excluded[int64(key[0])]; skip {
			continue
		}
		if netUnits, ok := nets[key]; ok && netUnits != 0 {
			continue // covered by the wal_vs_pg diff above
		}
		if !signed.IsZero() {
			out = append(out, AmountFinding(CatPositions,
				positionSubject(int64(key[0]), int64(key[1])),
				"pg_vs_wal", signed, decimal.Zero, UnitQty).
				WithDetail(map[string]any{
					"reason": "projection holds a position the engine journal never produced",
				}).WithHalt("ACCOUNT", strconv.FormatInt(int64(key[0]), 10)))
		}
	}
	return out
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
