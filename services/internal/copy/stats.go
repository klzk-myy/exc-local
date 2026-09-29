package copy

import (
	"context"
	"sort"

	"exchange/pkg/decimal"
)

// RateSource converts 1 unit of `base` into `quote` currency at the
// venue's reference (index) rate — the settlement.IndexPricer /
// settlement.ConverterIndexPricer seam (Task 3.3.9 multi-currency
// converter). Discovery fails closed on an unresolved path rather than
// fabricating a cross rate.
type RateSource interface {
	IndexRate(ctx context.Context, base, quote string) (decimal.Decimal, error)
}

// lot is one open lot in the FIFO book (per instrument).
type lot struct {
	qty   decimal.Decimal // remaining open quantity (>0)
	price decimal.Decimal // entry price
	long  bool
}

// RealizedPnL computes the manager's realized P&L per instrument via FIFO
// lot matching (decimal-exact, deterministic — fills are replayed in
// their persisted (created_at, id) order) and returns the cumulative P&L
// curve points ordered by fill time. Open lots left at the tail count
// ZERO toward realized P&L — unrealized exposure is marked only when a
// mark-source lands; discovery then reports the realized track.
//
// All P&L is expressed in the instrument's QUOTE currency; conversion to
// the strategy denomination happens in ComputeStats via RateSource.
func realizedPnLCurve(fills []ManagerFill) []decimal.Decimal {
	books := map[int64][]lot{} // instrument_id → open lots (FIFO)
	curve := make([]decimal.Decimal, 0, len(fills))
	for _, f := range fills {
		bk := books[f.InstrumentID]
		remaining := f.Quantity
		pnl := decimal.Zero
		// Close against the FIFO book.
		for remaining.IsPositive() && len(bk) > 0 && bk[0].long != (f.Side == "BUY") {
			head := &bk[0]
			take := head.qty
			if remaining.LessThan(take) {
				take = remaining
			}
			if head.long {
				// long lot closed by a SELL: pnl = (close - open) × qty
				pnl = pnl.Add(f.Price.Sub(head.price).Mul(take))
			} else {
				pnl = pnl.Add(head.price.Sub(f.Price).Mul(take))
			}
			head.qty = head.qty.Sub(take)
			remaining = remaining.Sub(take)
			if head.qty.IsZero() {
				bk = bk[1:]
			}
		}
		if remaining.IsPositive() {
			bk = append(bk, lot{qty: remaining, price: f.Price, long: f.Side == "BUY"})
		}
		books[f.InstrumentID] = bk
		curve = append(curve, pnl)
	}
	return curve
}

// ComputedStats is the raw computed result before the follower overlay.
type ComputedStats struct {
	PnL         decimal.Decimal // Σ realized P&L in strategy currency
	Notional    decimal.Decimal // Σ traded notional in strategy currency
	MaxDrawdown decimal.Decimal // max peak→trough of the P&L curve
}

// ComputeStats derives return/drawdown from the manager's persisted fills.
// Every cross-currency figure resolves through rates; a missing rate is a
// hard error — the strategy is excluded from discovery (fail closed,
// spec §12.9: computed-only).
func ComputeStats(ctx context.Context, fills []ManagerFill,
	strategyCcy string, rates RateSource) (*ComputedStats, error) {
	if rates == nil {
		return nil, errorf(CodeServiceDegraded, "stats rate source unavailable")
	}
	// Per-fill conversion of realized P&L + notional into strategy ccy.
	pnlSegs := realizedPnLCurve(fills)
	stats := &ComputedStats{PnL: decimal.Zero, Notional: decimal.Zero, MaxDrawdown: decimal.Zero}
	peak := decimal.Zero
	running := decimal.Zero
	for i, f := range fills {
		if f.QuoteCcy == "" {
			return nil, errorf(CodeServiceDegraded,
				"fill %d has no quote currency — cannot denominate P&L", f.TradeID)
		}
		rate := decimal.One
		if f.QuoteCcy != strategyCcy {
			r, err := rates.IndexRate(ctx, f.QuoteCcy, strategyCcy)
			if err != nil || !r.IsPositive() {
				return nil, errorf(CodeServiceDegraded,
					"no %s→%s index rate for fill %d — strategy stats uncomputable",
					f.QuoteCcy, strategyCcy, f.TradeID)
			}
			rate = r
		}
		stats.PnL = stats.PnL.Add(pnlSegs[i].Mul(rate))
		stats.Notional = stats.Notional.Add(f.Quantity.Mul(f.Price).Mul(rate))
		running = running.Add(pnlSegs[i].Mul(rate))
		if running.GreaterThan(peak) {
			peak = running
		}
		if dd := peak.Sub(running); dd.GreaterThan(stats.MaxDrawdown) {
			stats.MaxDrawdown = dd
		}
	}
	return stats, nil
}

// ReturnPct is realized P&L ÷ Σ traded notional (a deterministic,
// fill-derived track ratio — self-reported performance never enters).
func (c *ComputedStats) ReturnPct() decimal.Decimal {
	if !c.Notional.IsPositive() {
		return decimal.Zero
	}
	return c.PnL.Mul(decimal.NewFromInt(100)).Div(c.Notional)
}

// SortStrategiesByReturn orders a stats slice by descending return —
// the discovery ranking is computed, never curated. strategy_id breaks
// ties for a stable published order.
func SortStrategiesByReturn(stats []StrategyStats) {
	sort.Slice(stats, func(i, j int) bool {
		if c := stats[i].ReturnPct.Cmp(stats[j].ReturnPct); c != 0 {
			return c > 0
		}
		return stats[i].StrategyID < stats[j].StrategyID
	})
}
