/**
 * Display formatting for market/account values. Instrument-aware:
 * fraction digits derive from the instrument's tick_size / lot_size
 * (§21.12 item 2 — JPY-pair 3-decimal vs standard 5-decimal pricing is
 * exactly tick_size precision). Pure functions; no locale tables (en-US
 * grouping only, spec §27 R5).
 */
import { Dec, decOrZero, stepDecimals } from './decimal';
import type { Instrument } from './wire';

/** Group the integer part of a canonical decimal string: "1234567.89" →
 * "1,234,567.89". */
export function groupThousands(s: string): string {
  const neg = s.startsWith('-');
  const body = neg ? s.slice(1) : s;
  const dot = body.indexOf('.');
  const int = dot === -1 ? body : body.slice(0, dot);
  const frac = dot === -1 ? '' : body.slice(dot);
  const grouped = int.replace(/\B(?=(\d{3})+(?!\d))/g, ',');
  return `${neg ? '-' : ''}${grouped}${frac}`;
}

/** Format a wire decimal at `places` fraction digits with en-US grouping. */
export function formatDecimal(value: string | Dec | null | undefined, places: number): string {
  const d = typeof value === 'object' && value !== null ? value : decOrZero(value);
  return groupThousands(d.toFixed(places));
}

/** Price at the instrument's tick precision. */
export function formatPrice(value: string | Dec | null | undefined, inst: Instrument): string {
  return formatDecimal(value, stepDecimals(inst.tickSize));
}

/** Quantity at the instrument's lot precision. */
export function formatQty(value: string | Dec | null | undefined, inst: Instrument): string {
  return formatDecimal(value, stepDecimals(inst.lotSize));
}

/** Currency amount at 2dp (fiat convention). */
export function formatCurrency(value: string | Dec | null | undefined, places = 2): string {
  return formatDecimal(value, places);
}

/** Signed P&L: "+12.34" / "-5.00" / "0.00" (zero renders unsigned). */
export function formatPnl(value: string | Dec | null | undefined, places = 2): string {
  const d = typeof value === 'object' && value !== null ? value : decOrZero(value);
  if (d.isPositive()) return `+${groupThousands(d.toFixed(places))}`;
  return groupThousands(d.toFixed(places));
}

/** Normalize a user-typed numeric input: trims, rejects empty/garbage,
 * returns canonical decimal string or null. */
export function parseInputDecimal(raw: string): Dec | null {
  const s = raw.trim().replace(/,/g, '');
  if (s === '') return null;
  return Dec.parse(s);
}
