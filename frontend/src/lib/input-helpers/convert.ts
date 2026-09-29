/**
 * Unit converters (Task 10.3.29 item 10) — pure fixed-point functions
 * consumed by the calculator widget (10.3.8), percentage sliders
 * (10.3.11) and the preview engine (item 3).
 *
 * All inputs/outputs are decimal strings (§5.3). Every converter
 * returns null on malformed input — callers surface a validation error.
 */
import { divDecimal, mulDecimal, roundToStep } from './decimal';
import { STANDARD_LOT_UNITS, type InstrumentMeta } from './instruments';

/** pip ↔ price via instrument pipSize. */
export function pipToPrice(meta: InstrumentMeta, pips: string): string | null {
  return mulDecimal(pips, meta.pipSize);
}
export function priceToPip(meta: InstrumentMeta, price: string): string | null {
  return divDecimal(price, meta.pipSize, 8);
}

/** lots ↔ base-currency units via contract size (default 100,000 —
 * StandardLotUnits until migration 087 lands per-instrument values). */
export function lotsToUnits(meta: InstrumentMeta | undefined, lots: string): string | null {
  const cs = meta?.contractSize ?? STANDARD_LOT_UNITS;
  return mulDecimal(lots, cs);
}
export function unitsToLots(meta: InstrumentMeta | undefined, units: string): string | null {
  const cs = meta?.contractSize ?? STANDARD_LOT_UNITS;
  return divDecimal(units, cs, 8);
}

/** base ↔ quote at a given rate (live mark/reference price). */
export function baseToQuote(
  _meta: InstrumentMeta | undefined,
  baseAmount: string,
  rate: string,
): string | null {
  return mulDecimal(baseAmount, rate);
}
export function quoteToBase(
  meta: InstrumentMeta | undefined,
  quoteAmount: string,
  rate: string,
): string | null {
  const base = divDecimal(quoteAmount, rate, 12, 'down');
  if (base === null || meta === undefined) return base;
  // Lot-round DOWN — the conversion can never exceed the ask (mirrors
  // orders/service.go quote-denominated market conversion).
  return roundToStep(base, meta.lotSize, 'down') ?? base;
}

/** Account-currency conversion via the daily P&L conversion rate
 * (Phase-03 Task 3.3.9 supplies the rate; here it's a parameter so the
 * function stays pure/testable). */
export function convertToAccountCurrency(amount: string, rate: string): string | null {
  return mulDecimal(amount, rate);
}

/** % of free margin → quantity: floor to the lot grid. Mirrors the
 * slider math of Task 10.3.11 as a shared primitive:
 *   qty = (freeMargin × pct% × leverage) / price  — then lot-round down. */
export function pctToQty(
  meta: InstrumentMeta,
  pct: string,
  freeMargin: string,
  leverage: string,
  price: string,
): string | null {
  const marginSlice = mulDecimal(freeMargin, divDecimal(pct, '100', 8) ?? '0');
  if (marginSlice === null) return null;
  const notional = mulDecimal(marginSlice, leverage);
  if (notional === null) return null;
  const qty = divDecimal(notional, price, 12, 'down');
  if (qty === null) return null;
  return roundToStep(qty, meta.lotSize, 'down') ?? qty;
}

/** Inverse of pctToQty: qty → % of free margin at leverage/price. */
export function qtyToPct(
  _meta: InstrumentMeta,
  qty: string,
  freeMargin: string,
  leverage: string,
  price: string,
): string | null {
  const notional = mulDecimal(qty, price);
  if (notional === null) return null;
  const marginUsed = divDecimal(notional, leverage, 12);
  if (marginUsed === null) return null;
  const pct = divDecimal(marginUsed, freeMargin, 8);
  if (pct === null) return null;
  return mulDecimal(pct, '100');
}
