/**
 * Projected-execution math (Task 10.3.10 confirmation modals) and the
 * cumulative-depth curve (Task 10.3.12) — pure Dec math over the
 * canonical L2 book (marketdata depthUpdate / marketapi.BookSnapshot).
 */
import { Dec } from '@/lib/decimal/decimal';
import type { BookLevel, BookSnapshot } from './types';

/** Walk the book for `qty` against the given side; returns the honest
 * estimate or `undefined` when the book/qty can't support a projection
 * (empty book, zero qty) — never fabricate a fill price. */
export interface FillProjection {
  /** Volume-weighted average fill price across the walked levels. */
  avgPrice: Dec;
  /** Total quantity the visible book can fill (< requested if shallow). */
  filledQty: Dec;
  /** Whole request fillable at visible depth. */
  fullyFillable: boolean;
  /** Slippage vs mid, in basis points (undefined without a mid). */
  slippageBps: Dec | undefined;
}

export function projectFill(
  book: BookSnapshot | undefined,
  side: 'BUY' | 'SELL',
  qty: Dec,
  mid: Dec | undefined,
): FillProjection | undefined {
  if (!book || qty.isZero() || qty.isNegative()) return undefined;
  const levels = side === 'BUY' ? book.asks : book.bids;
  if (levels.length === 0) return undefined;
  let remaining = qty;
  let cost = Dec.ZERO;
  for (const lvl of levels) {
    if (remaining.isZero()) break;
    const take = lvl.qty.gte(remaining) ? remaining : lvl.qty;
    cost = cost.add(lvl.price.mul(take));
    remaining = remaining.sub(take);
  }
  const filled = qty.sub(remaining);
  if (filled.isZero()) return undefined;
  const avg = cost.div(filled);
  let slippage: Dec | undefined;
  if (mid?.isPositive()) {
    const diff = side === 'BUY' ? avg.sub(mid) : mid.sub(avg);
    slippage = diff.div(mid).mul(Dec.of(10_000)).abs();
  }
  return {
    avgPrice: avg,
    filledQty: filled,
    fullyFillable: remaining.isZero(),
    slippageBps: slippage,
  };
}

/** Mid from a book's best bid/ask — undefined on one-sided/empty books. */
export function bookMid(book: BookSnapshot | undefined): Dec | undefined {
  const bid = book?.bids[0]?.price;
  const ask = book?.asks[0]?.price;
  if (!bid || !ask) return undefined;
  return bid.add(ask).div(Dec.of(2));
}

// ---------------------------------------------------------------------------
// Depth chart — cumulative curves (price → cumulative qty / notional)
// ---------------------------------------------------------------------------

export interface DepthPoint {
  price: Dec;
  /** Cumulative base qty available at this price-or-better. */
  cumQty: Dec;
  /** Cumulative quote notional. */
  cumNotional: Dec;
}

/** Cumulate levels outward from best price. Bids must arrive sorted
 * desc, asks asc (the canonical L2 order). */
export function cumulate(levels: readonly BookLevel[]): DepthPoint[] {
  const out: DepthPoint[] = [];
  let q = Dec.ZERO;
  let n = Dec.ZERO;
  for (const lvl of levels) {
    q = q.add(lvl.qty);
    n = n.add(lvl.price.mul(lvl.qty));
    out.push({ price: lvl.price, cumQty: q, cumNotional: n });
  }
  return out;
}

/** Cumulative qty + notional at an arbitrary price (for crosshair
 * readouts): the depth at "price or better" on the given side. */
export function depthAtPrice(
  side: 'BID' | 'ASK',
  price: Dec,
  book: BookSnapshot | undefined,
): { qty: Dec; notional: Dec } {
  const levels = side === 'BID' ? book?.bids : book?.asks;
  let qty = Dec.ZERO;
  let notional = Dec.ZERO;
  for (const lvl of levels ?? []) {
    const inRange = side === 'BID' ? lvl.price.gte(price) : lvl.price.lte(price);
    if (!inRange) break;
    qty = qty.add(lvl.qty);
    notional = notional.add(lvl.price.mul(lvl.qty));
  }
  return { qty, notional };
}
