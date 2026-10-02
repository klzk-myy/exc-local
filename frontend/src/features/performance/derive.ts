/**
 * Client-side performance derivation (Phase-10 Task 10.3.18 fallback
 * path). The aggregate reporting endpoints — /api/v1/account/pnl,
 * /account/income, /account/snapshots, /reports/tca — are live
 * (Phase-13/Phase-20 owners); this module remains the fallback the
 * page uses when a probe reports unavailable. Live surfaces:
 *
 *   GET /api/v1/positions   open positions (realized + unrealized P&L)
 *   GET /api/v1/orders      order history (filled qty × avg_fill_price)
 *
 * DERIVATION RULES (all labeled "derived" in the UI):
 *   - Realized P&L per pair: matched buys-vs-sells by FILL PRICE under a
 *     FIFO proxy — sells beyond cumulative buys realize (avgFill −
 *     vwapBuy) × qty. This is a display approximation, not a ledger.
 *   - Win rate: share of closed-quantity lots with positive realized P&L.
 *   - Fees: commissions are not broken out on the order wire — the total
 *     shown is `filled_notional × commission_bps` ONLY when the account's
 *     effective commission surface supplies a rate; otherwise "—".
 *   - Equity curve: positions' realized+unrealized sums — no historical
 *     snapshots endpoint exists yet, so the curve is current-state only
 *     and the chart section says so.
 */
import type { PositionRow } from '@/lib/market/wire';

export interface OrderRow {
  orderId: string;
  side: string;
  status: string;
  filledQty: string;
  avgFillPrice?: string;
  symbol?: string;
  instrumentId: number;
  createdAt: string;
}

export interface PairStats {
  symbol: string;
  filledBuyQty: number;
  filledSellQty: number;
  realizedPnl: number;
  wins: number;
  losses: number;
  notional: number;
  unrealizedPnl: number;
}

export interface PerformanceSummary {
  realizedPnl: number;
  unrealizedPnl: number;
  winRate: number | null;
  perPair: PairStats[];
  filledOrders: number;
  derived: true;
}

const N = (s: string | undefined): number => {
  if (s === undefined) return 0;
  const n = Number(s);
  return Number.isFinite(n) ? n : 0;
};

/** Pairing approximation: walk filled orders chronologically; every sell
 * lot realizes against the running buy VWAP (and vice-versa for shorts). */
export function derivePerformance(
  orders: readonly OrderRow[],
  positions: readonly PositionRow[],
): PerformanceSummary {
  const bySymbol = new Map<string, OrderRow[]>();
  for (const o of orders) {
    if (o.status !== 'FILLED' && o.status !== 'PARTIALLY_FILLED') continue;
    if (N(o.filledQty) <= 0 || N(o.avgFillPrice) <= 0) continue;
    const sym = o.symbol ?? `instrument:${o.instrumentId}`;
    const list = bySymbol.get(sym) ?? [];
    list.push(o);
    bySymbol.set(sym, list);
  }

  const perPair: PairStats[] = [];
  let realized = 0;
  let wins = 0;
  let losses = 0;

  for (const [symbol, list] of bySymbol) {
    list.sort((a, b) => a.createdAt.localeCompare(b.createdAt));
    let netQty = 0; // signed base qty accumulated
    let costBasis = 0; // quote units spent on netQty
    let pairRealized = 0;
    let buyQty = 0;
    let sellQty = 0;
    let notional = 0;
    for (const o of list) {
      const qty = N(o.filledQty);
      const px = N(o.avgFillPrice);
      const value = qty * px;
      notional += value;
      if (o.side === 'BUY') {
        buyQty += qty;
        if (netQty < 0) {
          // covering a short — realize (basisPerUnit − px) × coveredQty
          const covered = Math.min(qty, -netQty);
          const basis = netQty !== 0 ? costBasis / -netQty : 0;
          const pnl = (basis - px) * covered;
          pairRealized += pnl;
          if (pnl > 0) wins++;
          else if (pnl < 0) losses++;
          costBasis += basis * covered; // release covered basis (was negative)
          netQty += covered;
          const rest = qty - covered;
          if (rest > 0) {
            netQty += rest;
            costBasis += rest * px;
          }
        } else {
          netQty += qty;
          costBasis += value;
        }
      } else if (o.side === 'SELL') {
        sellQty += qty;
        if (netQty > 0) {
          const closedQty = Math.min(qty, netQty);
          const basis = costBasis / netQty;
          const pnl = (px - basis) * closedQty;
          pairRealized += pnl;
          if (pnl > 0) wins++;
          else if (pnl < 0) losses++;
          costBasis -= basis * closedQty;
          netQty -= closedQty;
          const rest = qty - closedQty;
          if (rest > 0) {
            netQty -= rest;
            costBasis -= rest * px;
          }
        } else {
          netQty -= qty;
          costBasis -= value;
        }
      }
    }
    realized += pairRealized;
    perPair.push({
      symbol,
      filledBuyQty: buyQty,
      filledSellQty: sellQty,
      realizedPnl: pairRealized,
      wins: 0,
      losses: 0,
      notional,
      unrealizedPnl: 0,
    });
  }

  let unrealized = 0;
  for (const p of positions) {
    const u = N(p.unrealizedPnl);
    const r = N(p.realizedPnl);
    unrealized += u;
    realized += r;
    const row = perPair.find((x) => x.symbol === p.symbol);
    if (row !== undefined) {
      row.unrealizedPnl += u;
      row.realizedPnl += r;
    } else {
      perPair.push({
        symbol: p.symbol,
        filledBuyQty: 0,
        filledSellQty: 0,
        realizedPnl: r,
        wins: 0,
        losses: 0,
        notional: 0,
        unrealizedPnl: u,
      });
    }
  }

  const decided = wins + losses;
  return {
    realizedPnl: realized,
    unrealizedPnl: unrealized,
    winRate: decided > 0 ? wins / decided : null,
    perPair: perPair.sort((a, b) => Math.abs(b.realizedPnl) - Math.abs(a.realizedPnl)),
    filledOrders: orders.filter((o) => N(o.filledQty) > 0).length,
    derived: true,
  };
}
