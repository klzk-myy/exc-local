/**
 * String-in / string-out decimal helpers for the input-helper framework.
 *
 * This module is a thin functional wrapper over the shared `Dec` engine
 * (`@/lib/decimal/decimal`) — there is exactly ONE bigint-decimal
 * implementation in the codebase; the helpers here expose the
 * string-oriented shape (`string → string | null`) that forms, parsers
 * and validators consume.
 *
 * Fail-closed rule: every helper returns null on malformed input —
 * callers surface a validation error rather than silently coercing.
 */
import { Dec } from '@/lib/decimal/decimal';

export interface DecimalParts {
  /** Signed mantissa scaled by `scale` (value = mantissa / 10^scale). */
  readonly mantissa: bigint;
  readonly scale: number;
}

/** Parse a decimal string ("1.2345", "-0.001", "12", "1e-4") into parts.
 * Returns null for anything that is not a plain/scientific decimal. */
export function parseDecimal(raw: string): DecimalParts | null {
  const d = Dec.tryOf(raw);
  if (d === undefined) return null;
  return { mantissa: d.units, scale: d.scale };
}

export function isDecimal(raw: string): boolean {
  return Dec.tryOf(raw) !== undefined;
}

/** Number of fractional digits in the canonical decimal string. */
export function decimalPlaces(raw: string): number {
  return Dec.tryOf(raw)?.scale ?? 0;
}

/** Render parts back to a trimmed decimal string. */
export function toDecimalString(d: DecimalParts): string {
  const neg = d.mantissa < 0n;
  const digits = (neg ? -d.mantissa : d.mantissa).toString();
  if (d.scale === 0) return (neg ? '-' : '') + digits;
  const padded = digits.padStart(d.scale + 1, '0');
  const int = padded.slice(0, padded.length - d.scale);
  const frac = padded.slice(padded.length - d.scale).replace(/0+$/, '');
  return (neg ? '-' : '') + int + (frac.length > 0 ? `.${frac}` : '');
}

export function addDecimal(a: string, b: string): string | null {
  const da = Dec.tryOf(a);
  const db = Dec.tryOf(b);
  if (!da || !db) return null;
  return da.add(db).toString();
}

export function subDecimal(a: string, b: string): string | null {
  const da = Dec.tryOf(a);
  const db = Dec.tryOf(b);
  if (!da || !db) return null;
  return da.sub(db).toString();
}

export function mulDecimal(a: string, b: string): string | null {
  const da = Dec.tryOf(a);
  const db = Dec.tryOf(b);
  if (!da || !db) return null;
  return da.mul(db).toString();
}

export type RoundingMode = 'down' | 'up' | 'half-up';

function pow10(n: number): bigint {
  return 10n ** BigInt(n);
}

/** Divide two decimal strings with `scale` fractional digits of
 * precision, rounded per `mode` (default half-up). Returns null on
 * malformed input or division by zero. */
export function divDecimal(
  a: string,
  b: string,
  scale = 18,
  mode: RoundingMode = 'half-up',
): string | null {
  const da = Dec.tryOf(a);
  const db = Dec.tryOf(b);
  if (!da || !db || db.isZero()) return null;
  if (mode === 'half-up') return da.div(db, scale).toString();
  // Floor/ceil modes: exact rational arithmetic at the target scale.
  const num = da.units * pow10(db.scale + scale);
  const den = db.units * pow10(da.scale);
  let q = num / den;
  const rem = num % den;
  if (rem !== 0n) {
    const negative = num < 0n !== den < 0n;
    // 'down' = floor (needs one more toward -∞ when negative),
    // 'up' = ceil (one more toward +∞ when positive).
    if (mode === 'down' && negative) q -= 1n;
    else if (mode === 'up' && !negative) q += 1n;
  }
  return toDecimalString({ mantissa: q, scale });
}

/** -1 / 0 / +1 comparison. Null on malformed input. */
export function cmpDecimal(a: string, b: string): -1 | 0 | 1 | null {
  const da = Dec.tryOf(a);
  const db = Dec.tryOf(b);
  if (!da || !db) return null;
  return da.cmp(db) as -1 | 0 | 1;
}

export function isPositive(raw: string): boolean {
  return Dec.tryOf(raw)?.isPositive() === true;
}

export function isZero(raw: string): boolean {
  return Dec.tryOf(raw)?.isZero() === true;
}

/** True when `value` is an exact multiple of `step` (tick/lot check —
 * mirrors validate.go `q.Mod(inst.LotSize).IsZero()`). */
export function isMultipleOf(value: string, step: string): boolean {
  const v = Dec.tryOf(value);
  const s = Dec.tryOf(step);
  if (!v || !s?.isPositive()) return false;
  // quantizeTo('down') lands on the largest multiple ≤ v; equal iff exact.
  return v.quantizeTo(s, 'down').eq(v);
}

/** Round `value` to a multiple of `step`. 'down' floors toward the step
 * grid (lot-rounding for market conversions), 'up' ceils, 'nearest'
 * rounds to the closest multiple (half ties go up). */
export function roundToStep(
  value: string,
  step: string,
  mode: 'down' | 'up' | 'nearest' = 'nearest',
): string | null {
  const v = Dec.tryOf(value);
  const s = Dec.tryOf(step);
  if (!v || !s?.isPositive()) return null;
  return v.quantizeTo(s, mode).toString();
}

/** Clamp a decimal to [min, max] (either bound may be null). */
export function clampDecimal(value: string, min: string | null, max: string | null): string {
  let v = value;
  if (min !== null && cmpDecimal(v, min) === -1) v = min;
  if (max !== null && cmpDecimal(v, max) === 1) v = max;
  return v;
}
