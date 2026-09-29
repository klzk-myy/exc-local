/**
 * L2 order-book model (Task 10.3.2) — pure functions over wire shapes.
 *
 * Both surfaces carry the aggregated top-N book:
 *   - REST  GET /api/v1/book/{symbol}?depth=N → BookSnapshotWire
 *   - WS    depth@{symbol}:{levels}:{ms}      → DepthUpdate frames
 *     (conflated by the server, spec §10.1/§10.2)
 *
 * Semantics are REPLACE, not delta-apply: every frame carries the full
 * level set for its depth variant (services/internal/marketdata/l2.go).
 * Sparse books emit fewer levels — consumers must not pad (§10.1, §6.6).
 */
import { Dec, decOrZero } from '@/lib/market/decimal';
import type { BookLevel, BookSnapshotWire, DepthUpdate } from '@/lib/market/wire';

export interface BookState {
  symbol: string;
  /** Best-first: bids sorted desc, asks sorted asc. Same-price levels are
   * merged (summed) — level aggregation is idempotent under replace. */
  bids: BookLevel[];
  asks: BookLevel[];
  /** Stream cursor (WS last_seq / REST seq). */
  seq: number;
  updatedAtMs: number;
}

export const EMPTY_BOOK: BookState = { symbol: '', bids: [], asks: [], seq: 0, updatedAtMs: 0 };

/** Merge same-price levels (qty summed, count summed) and sort best-first. */
export function aggregateLevels(levels: readonly BookLevel[], side: 'bids' | 'asks'): BookLevel[] {
  const byPrice = new Map<string, { qty: Dec; count: number }>();
  for (const l of levels) {
    const qty = decOrZero(l.qty);
    if (qty.isZero()) continue; // qty=0 level = removed level
    const cur = byPrice.get(l.price);
    if (cur) {
      cur.qty = cur.qty.add(qty);
      cur.count += l.count ?? 0;
    } else {
      byPrice.set(l.price, { qty, count: l.count ?? 0 });
    }
  }
  const out: BookLevel[] = [...byPrice.entries()].map(([price, v]) => ({
    price,
    qty: v.qty.toString(),
    count: v.count,
  }));
  out.sort((a, b) => {
    const c = decOrZero(a.price).cmp(decOrZero(b.price));
    return side === 'bids' ? -c : c; // bids desc, asks asc
  });
  return out;
}

export function applySnapshot(snap: BookSnapshotWire, levels: number): BookState {
  return {
    symbol: snap.symbol,
    bids: aggregateLevels(snap.bids, 'bids').slice(0, levels),
    asks: aggregateLevels(snap.asks, 'asks').slice(0, levels),
    seq: snap.seq,
    updatedAtMs: snap.updatedAtMs,
  };
}

/** Apply one WS frame. Older/equal frames are dropped (seq guard) so a
 * replayed ring-buffer frame never regresses the view. */
export function applyDepthUpdate(state: BookState, upd: DepthUpdate, levels: number): BookState {
  if (state.symbol !== '' && upd.symbol !== state.symbol) return state;
  if (upd.lastSeq > 0 && state.seq > 0 && upd.lastSeq <= state.seq && !upd.isSnapshot) {
    return state;
  }
  return {
    symbol: upd.symbol,
    bids: aggregateLevels(upd.bids, 'bids').slice(0, levels),
    asks: aggregateLevels(upd.asks, 'asks').slice(0, levels),
    seq: upd.lastSeq > 0 ? upd.lastSeq : upd.seq,
    updatedAtMs: upd.tsMs,
  };
}

// ---------------------------------------------------------------------------
// Derived view
// ---------------------------------------------------------------------------

export interface BookRow {
  key: string;
  side: 'bid' | 'ask';
  price: string;
  qty: string;
  /** Cumulative qty from best to this level. */
  total: string;
  /** total / maxTotal — drives the cumulative depth bar. */
  depthFrac: number;
  count?: number;
}

export interface BookView {
  bids: BookRow[];
  asks: BookRow[];
  /** best ask − best bid (null when a side is empty, §6.6 sparse book). */
  spread: Dec | null;
  mid: Dec | null;
  /** Cumulative max across BOTH sides — consistent bar scale. */
  maxTotal: Dec;
  seq: number;
  stale: boolean;
}

export function deriveBookView(state: BookState): BookView {
  function rows(levels: readonly BookLevel[], side: 'bid' | 'ask'): { rows: BookRow[]; max: Dec } {
    let cum = Dec.ZERO;
    const rows: BookRow[] = [];
    for (const l of levels) {
      const qty = decOrZero(l.qty);
      cum = cum.add(qty);
      rows.push({
        key: `${side}:${l.price}`,
        side,
        price: l.price,
        qty: l.qty,
        total: cum.toString(),
        depthFrac: 0, // filled once max is known
        count: l.count,
      });
    }
    return { rows, max: cum };
  }
  const b = rows(state.bids, 'bid');
  const a = rows(state.asks, 'ask');
  const maxTotal = b.max.gte(a.max) ? b.max : a.max;
  const maxN = maxTotal.toNumber();
  for (const r of [...b.rows, ...a.rows]) {
    r.depthFrac = maxN > 0 ? decOrZero(r.total).toNumber() / maxN : 0;
  }

  const bestBid = state.bids[0] ? decOrZero(state.bids[0].price) : null;
  const bestAsk = state.asks[0] ? decOrZero(state.asks[0].price) : null;
  const spread = bestBid && bestAsk ? bestAsk.sub(bestBid) : null;
  const mid = bestBid && bestAsk ? bestAsk.add(bestBid).divInt(2, 1) : null;

  return {
    bids: b.rows,
    asks: a.rows,
    spread,
    mid,
    maxTotal,
    seq: state.seq,
    stale: false,
  };
}

/** Signed per-level qty deltas between consecutive views — feeds the
 * per-level flash highlight (price/qty appearance = 'up', shrink/vanish
 * = 'down'). Returns a map keyed by row key. */
export function diffLevels(prev: BookState, next: BookState): Map<string, 'up' | 'down'> {
  const out = new Map<string, 'up' | 'down'>();
  for (const side of ['bid', 'ask'] as const) {
    const key = side === 'bid' ? 'bids' : 'asks';
    const before = new Map<string, string>();
    for (const l of prev[key]) before.set(l.price, l.qty);
    const seen = new Set<string>();
    for (const l of next[key]) {
      seen.add(l.price);
      const prior = before.get(l.price);
      if (prior === undefined) {
        out.set(`${side}:${l.price}`, 'up');
      } else {
        const c = decOrZero(l.qty).cmp(decOrZero(prior));
        if (c > 0) out.set(`${side}:${l.price}`, 'up');
        else if (c < 0) out.set(`${side}:${l.price}`, 'down');
      }
    }
    for (const l of prev[key]) {
      if (!seen.has(l.price)) out.set(`${side}:${l.price}`, 'down');
    }
  }
  return out;
}
