/**
 * FX pair helpers — symbol grammar is `BASE/QUOTE` (e.g. `EUR/USD`).
 * Pip conventions: 4th decimal for most pairs (0.0001), 2nd decimal for
 * JPY-quoted pairs (0.01) — matching the Phase-03 pip engine (Task
 * 3.3.12). Where the instrument's tick_size is finer than the canonical
 * pip (fractional-pip pricing), the pip stays canonical.
 */
import { Dec, dec, tryDec } from '@/lib/decimal/decimal';
import type { Instrument } from './types';

export interface Pair {
  base: string;
  quote: string;
}

/** Split `EUR/USD` → {base:'EUR', quote:'USD'}; tolerates bare `EURUSD`
 * (6-letter fiat code) as a fallback. */
export function splitPair(symbol: string): Pair | null {
  const slash = symbol.indexOf('/');
  if (slash > 0) {
    const base = symbol.slice(0, slash);
    const quote = symbol.slice(slash + 1);
    if (base && quote) return { base, quote };
    return null;
  }
  if (/^[A-Z]{6}$/i.test(symbol)) {
    return { base: symbol.slice(0, 3).toUpperCase(), quote: symbol.slice(3).toUpperCase() };
  }
  return null;
}

export function isJpyPair(symbol: string): boolean {
  return splitPair(symbol)?.quote === 'JPY';
}

/** Canonical pip size for the pair (0.01 JPY-quoted, else 0.0001). */
export function pipSize(symbol: string): Dec {
  return isJpyPair(symbol) ? dec('0.01') : dec('0.0001');
}

/**
 * Pip value of one unit of `qty` in the pair's QUOTE currency:
 *   pipValue = qty × pipSize
 * (e.g. 1 lot = 100,000 EUR/USD → $10/pip; 1 lot USD/JPY → ¥1,000/pip).
 * Conversion to account currency is the caller's concern — it needs the
 * relevant conversion mark, which callers fetch or disclose as "quote
 * currency" output.
 */
export function pipValue(symbol: string, qty: Dec): Dec {
  return qty.mul(pipSize(symbol));
}

/**
 * Liquidation estimate — long/short price at which losses consume the
 * posted margin, per the venue's maintenance-margin model:
 *   liq = entry ∓ (margin − maintenanceRate·notional) / qty
 * Returns undefined when inputs are missing/zero — never fabricated.
 */
export function liquidationEstimate(
  side: 'LONG' | 'SHORT',
  entry: Dec,
  qty: Dec,
  margin: Dec,
  maintenanceRateBps: Dec,
): Dec | undefined {
  if (qty.isZero() || margin.isZero()) return undefined;
  const notional = entry.mul(qty);
  const maintenance = notional.mul(maintenanceRateBps).div(dec('10000'));
  const headroom = margin.sub(maintenance);
  const perUnit = headroom.div(qty);
  const liq = side === 'LONG' ? entry.sub(perUnit) : entry.add(perUnit);
  return liq.isPositive() ? liq : Dec.ZERO;
}

/** Price decimals to display — JPY pairs 3dp, others 5dp, unless the
 * instrument tick size implies a finer resolution. */
export function priceDecimals(symbol: string, tickSize?: Dec): number {
  if (tickSize?.isPositive()) {
    // e.g. 0.00001 → 5 decimals
    const s = tickSize.toString();
    const dot = s.indexOf('.');
    if (dot >= 0) return s.length - dot - 1;
  }
  return isJpyPair(symbol) ? 3 : 5;
}

/** Quantity decimals implied by the lot size (0.01 lots → 2dp). */
export function qtyDecimals(inst?: Instrument): number {
  if (!inst?.lotSize.isPositive()) return 2;
  const s = inst.lotSize.toString();
  const dot = s.indexOf('.');
  return dot >= 0 ? s.length - dot - 1 : 0;
}

/** Parse a user-typed numeric field — empty strings are "absent", not 0. */
export function parseInput(v: string): Dec | undefined {
  return v.trim() === '' ? undefined : tryDec(v);
}

/** Format a price for display with the pair's pip convention. */
export function formatPrice(symbol: string, price: Dec | undefined, tickSize?: Dec): string {
  if (!price) return '—';
  return price.toDisplay(priceDecimals(symbol, tickSize));
}

/** Effective lot/step/min-notional constraint view for sizing helpers. */
export interface QtyConstraints {
  step: Dec;
  minQty: Dec;
  maxQty: Dec;
  minNotional: Dec;
}

export function qtyConstraints(inst: Instrument | undefined): QtyConstraints {
  return {
    step: inst?.lotSize ?? dec('0.01'),
    minQty: inst?.minQty ?? Dec.ZERO,
    maxQty: inst?.maxQty ?? Dec.ZERO,
    minNotional: inst?.minNotional ?? Dec.ZERO,
  };
}
