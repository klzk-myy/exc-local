/**
 * Instrument-aware formatting (Task 10.3.29 item 2).
 *
 * `formatPrice`/`formatQty`/`formatCurrency`/`formatPips` are exported
 * for all components. All money math goes through decimal.ts — never
 * floats (spec §5.3 invariant 1).
 */
import {
  cmpDecimal,
  decimalPlaces,
  divDecimal,
  isDecimal,
  isPositive,
  mulDecimal,
  parseDecimal,
  roundToStep,
  toDecimalString,
} from './decimal';
import type { InstrumentMeta } from './instruments';

/** Render a decimal string at a fixed fractional precision (pad zeros,
 * round half-up on extra digits). Returns '—' on malformed input. */
export function formatFixed(raw: string, decimals: number): string {
  const d = parseDecimal(raw);
  if (d === null) return '—';
  const factor = 10n ** BigInt(d.scale);
  // scale to `decimals` digits via integer rounding
  const target = 10n ** BigInt(decimals);
  let scaled: bigint;
  if (d.scale <= decimals) {
    scaled = d.mantissa * (target / factor);
  } else {
    const divisor = 10n ** BigInt(d.scale - decimals);
    const half = divisor / 2n;
    const m = d.mantissa;
    scaled = m >= 0n ? (m + half) / divisor : (m - half) / divisor;
  }
  const neg = scaled < 0n;
  const digits = (neg ? -scaled : scaled).toString().padStart(decimals + 1, '0');
  const int = digits.slice(0, digits.length - decimals);
  const frac = decimals > 0 ? `.${digits.slice(digits.length - decimals)}` : '';
  return (neg ? '-' : '') + int + frac;
}

/** Thousands-grouped fixed-precision rendering (en-US locale grouping
 * is literal — no i18n per spec §27 R5). */
export function formatFixedGrouped(raw: string, decimals: number): string {
  const fixed = formatFixed(raw, decimals);
  if (fixed === '—') return fixed;
  const neg = fixed.startsWith('-');
  const body = neg ? fixed.slice(1) : fixed;
  const [int, frac] = body.split('.');
  const grouped = (int ?? '0').replace(/\B(?=(\d{3})+(?!\d))/g, ',');
  return (neg ? '-' : '') + grouped + (frac !== undefined ? `.${frac}` : '');
}

/** Price display: instrument tick precision (JPY pairs render 3dp via
 * tick_size=0.001; majors 5dp). `meta === undefined` → 5dp default. */
export function formatPrice(meta: InstrumentMeta | undefined, rawPrice: string): string {
  const dp = meta?.pricePrecision ?? 5;
  return formatFixed(rawPrice, dp);
}

/** Constrain a price input to the tick grid: snaps to the nearest tick
 * multiple. Returns null when the input is not a decimal. */
export function snapPriceToTick(meta: InstrumentMeta, rawPrice: string): string | null {
  if (!isDecimal(rawPrice)) return null;
  if (!isPositive(meta.tickSize)) return rawPrice;
  return roundToStep(rawPrice, meta.tickSize, 'nearest');
}

/** Keystroke-level price guard: rejects edits that would exceed the
 * instrument's price precision (sub-tick input is structurally invalid). */
export function isPriceKeystrokeAllowed(meta: InstrumentMeta | undefined, next: string): boolean {
  if (next === '' || next === '-' || next === '.') return true;
  if (!/^[+-]?\d*\.?\d*$/.test(next)) return false;
  const dp = meta?.pricePrecision ?? 5;
  const dot = next.indexOf('.');
  if (dot < 0) return true;
  return next.length - dot - 1 <= dp;
}

/** Quantity display: lot-precision (decimals of lot_size). */
export function formatQty(meta: InstrumentMeta | undefined, rawQty: string): string {
  const dp = meta?.qtyPrecision ?? 4;
  return formatFixed(rawQty, dp);
}

/** Snap a quantity to the lot grid (floor — never exceeds the ask,
 * mirroring the server's lot-round-down for quote conversions). */
export function snapQtyToLot(meta: InstrumentMeta, rawQty: string): string | null {
  if (!isDecimal(rawQty)) return null;
  if (!isPositive(meta.lotSize)) return rawQty;
  return roundToStep(rawQty, meta.lotSize, 'down');
}

const CURRENCY_DP: Record<string, number> = { JPY: 0, KRW: 0, HUF: 0 };

/** ISO-4217 currency rendering (2dp default; zero-decimal currencies
 * rendered whole). Symbol code prefix, not locale symbol, to keep it
 * unambiguous: "USD 1,234.56". */
export function formatCurrency(code: string, amount: string): string {
  const dp = CURRENCY_DP[code.toUpperCase()] ?? 2;
  return `${code.toUpperCase()} ${formatFixedGrouped(amount, dp)}`;
}

/** Price → pips for the instrument (price/pipSize). */
export function priceToPips(meta: InstrumentMeta, rawPrice: string): string {
  if (!isDecimal(rawPrice)) return '—';
  const pips = divDecimal(rawPrice, meta.pipSize, 2);
  return pips === null ? '—' : formatFixedGrouped(pips, 2);
}

/** Pips → price distance (pips*pipSize), rendered at price precision. */
export function pipsToPrice(meta: InstrumentMeta, pips: string): string {
  if (!isDecimal(pips)) return '—';
  const price = mulDecimal(pips, meta.pipSize);
  return price === null ? '—' : formatFixed(price, meta.pricePrecision);
}

/** Percent formatting for stats surfaces (2dp, e.g. "12.34%"). */
export function formatPct(raw: string, decimals = 2): string {
  const v = formatFixed(raw, decimals);
  return v === '—' ? v : `${v}%`;
}

/** Signed P&L display with color hint ('pos'|'neg'|'zero'). */
export function pnlTone(raw: string): 'pos' | 'neg' | 'zero' {
  const c = cmpDecimal(raw, '0');
  if (c === 1) return 'pos';
  if (c === -1) return 'neg';
  return 'zero';
}

/** Compact UTC timestamp rendering for tables. */
export function formatTimeMs(tsMs: number): string {
  return new Date(tsMs).toISOString().slice(11, 23);
}
export function formatDateTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toISOString().replace('T', ' ').slice(0, 19) + 'Z';
}

/** Strip trailing zeros / expose raw canonical decimal for inputs. */
export function canonicalDecimal(raw: string): string {
  const d = parseDecimal(raw);
  return d === null ? raw : toDecimalString(d);
}

/** Decimal-places count of an arbitrary decimal string (re-export so
 * consumers don't reach into decimal.ts for display helpers). */
export { decimalPlaces };
