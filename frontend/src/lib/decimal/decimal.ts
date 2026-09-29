/**
 * Exact decimal arithmetic — bigint-backed, arbitrary scale.
 *
 * The §5.3 wire contract renders every money/price/quantity as a decimal
 * string; JS `number` is a binary float and silently corrupts values like
 * `0.1 + 0.2`. UI-side calculations (margin, pip value, depth walks,
 * projected fills) therefore run on `Dec` and only convert to `number`
 * for display-only concerns (SVG coordinates, slider positions).
 *
 * Value model: `units × 10^(-scale)` — `Dec.parse("1.23")` is
 * `{ units: 123n, scale: 2 }`. Division produces a fixed-point result at
 * `DIV_SCALE` (18 decimals, half-up rounding) — more than the venue's
 * 1e-8 wire scale in every direction.
 */

const DIV_SCALE = 18;

function pow10(n: number): bigint {
  if (n < 0) throw new RangeError(`pow10(${n})`);
  return 10n ** BigInt(n);
}

export class Dec {
  /** Signed mantissa — value is `units * 10^(-scale)`. */
  readonly units: bigint;
  /** Decimal digits after the point in `units`. */
  readonly scale: number;

  private constructor(units: bigint, scale: number) {
    this.units = units;
    this.scale = scale;
  }

  /** `Dec.of(value)` — accepts decimal strings, integers, and Dec. */
  static of(v: string | number | bigint | Dec): Dec {
    const d = Dec.tryOf(v);
    if (d === undefined) throw new TypeError(`invalid decimal: ${String(v)}`);
    return d;
  }

  /** Lenient factory — returns undefined on malformed input instead of
   * throwing (wire payloads arrive as `unknown`). */
  static tryOf(v: string | number | bigint | Dec | null | undefined): Dec | undefined {
    if (v === null || v === undefined) return undefined;
    if (v instanceof Dec) return v;
    if (typeof v === 'bigint') return new Dec(v, 0);
    if (typeof v === 'number') {
      if (!Number.isFinite(v)) return undefined;
      return Dec.parse(String(v));
    }
    return Dec.parse(v);
  }

  private static parse(s: string): Dec | undefined {
    const t = s.trim();
    const m = /^([+-]?)(\d*)(?:\.(\d*))?(?:[eE]([+-]?\d+))?$/.exec(t);
    if (!m) return undefined;
    const digits = (m[2] ?? '') + (m[3] ?? '');
    if (digits.length === 0) return undefined;
    const exp = m[4] !== undefined ? parseInt(m[4], 10) : 0;
    const fracLen = (m[3] ?? '').length;
    const scale = fracLen - exp;
    const sign = m[1] === '-' ? -1n : 1n;
    const units = sign * BigInt(digits === '' ? '0' : digits);
    if (scale <= 0) return new Dec(units * pow10(-scale), 0);
    return new Dec(units, scale);
  }

  static readonly ZERO = new Dec(0n, 0);
  static readonly ONE = new Dec(1n, 0);

  // -- predicates ------------------------------------------------------------

  isZero(): boolean {
    return this.units === 0n;
  }
  isNegative(): boolean {
    return this.units < 0n;
  }
  isPositive(): boolean {
    return this.units > 0n;
  }

  // -- comparisons -------------------------------------------------------------

  cmp(other: Dec): number {
    const scale = Math.max(this.scale, other.scale);
    const a = this.units * pow10(scale - this.scale);
    const b = other.units * pow10(scale - other.scale);
    return a < b ? -1 : a > b ? 1 : 0;
  }
  lt(o: Dec): boolean {
    return this.cmp(o) < 0;
  }
  lte(o: Dec): boolean {
    return this.cmp(o) <= 0;
  }
  gt(o: Dec): boolean {
    return this.cmp(o) > 0;
  }
  gte(o: Dec): boolean {
    return this.cmp(o) >= 0;
  }
  eq(o: Dec): boolean {
    return this.cmp(o) === 0;
  }

  // -- arithmetic ---------------------------------------------------------------

  add(o: Dec): Dec {
    const scale = Math.max(this.scale, o.scale);
    return new Dec(
      this.units * pow10(scale - this.scale) + o.units * pow10(scale - o.scale),
      scale,
    );
  }
  sub(o: Dec): Dec {
    return this.add(o.neg());
  }
  neg(): Dec {
    return new Dec(-this.units, this.scale);
  }
  abs(): Dec {
    return this.units < 0n ? this.neg() : this;
  }
  mul(o: Dec): Dec {
    return new Dec(this.units * o.units, this.scale + o.scale);
  }
  /** a / b at `DIV_SCALE` fractional digits, half-up rounding.
   * Throws on division by zero (fail-closed — callers must gate). */
  div(o: Dec, outScale = DIV_SCALE): Dec {
    if (o.units === 0n) throw new RangeError('decimal division by zero');
    // value = a.u·10^-a.s / (b.u·10^-b.s) = a.u·10^(b.s) / (b.u·10^a.s)
    // result units at outScale: a.u·10^(b.s+outScale) / (b.u·10^a.s)
    const exp = o.scale + outScale - this.scale;
    let num = this.units;
    let den = o.units;
    if (exp >= 0) num *= pow10(exp);
    else den *= pow10(-exp);
    const q = num / den;
    const r = num % den;
    // Half-up on the remainder magnitude; direction follows the true sign
    // of num/den (q can be 0 while the true quotient is negative).
    const negative = num < 0n !== den < 0n;
    const absR = r < 0n ? -r : r;
    const absD = den < 0n ? -den : den;
    const rounded = absR * 2n >= absD ? q + (negative ? -1n : 1n) : q;
    return new Dec(rounded, outScale);
  }
  /** Multiply by a plain JS integer/decimal shortcut. */
  mulNumber(n: number): Dec {
    return this.mul(Dec.of(n));
  }

  /** Round to a multiple of `step` (lot-size / tick-size rounding for
   * order sizing): 'down' floors, 'up' ceils, 'nearest' rounds half-up.
   * A zero/negative step is a malformed filter — returns `this` so the
   * caller's filter check (not a crash) reports the problem. */
  quantizeTo(step: Dec, mode: 'down' | 'up' | 'nearest' = 'down'): Dec {
    if (!step.isPositive()) return this;
    // ratio = this/step at high precision; floor to an integer per mode.
    const ratio = this.div(step, DIV_SCALE + 4);
    const unit = pow10(ratio.scale);
    const u = ratio.units;
    const rem = u % unit;
    const floor = u / unit - (u < 0n && rem !== 0n ? 1n : 0n); // BigInt / truncates toward 0
    let q: bigint;
    switch (mode) {
      case 'down':
        q = floor;
        break;
      case 'up':
        q = floor + (rem !== 0n ? 1n : 0n);
        break;
      case 'nearest': {
        // |fractional part| ≥ 0.5 ⇒ round away from floor.
        const absRem = rem < 0n ? -rem : rem;
        q = absRem * 2n >= unit ? floor + 1n : floor;
        break;
      }
    }
    return new Dec(q, 0).mul(step);
  }

  // -- rendering ------------------------------------------------------------------

  /** Canonical decimal string — trailing fractional zeros trimmed. */
  toString(): string {
    const neg = this.units < 0n;
    const digits = (neg ? -this.units : this.units).toString().padStart(this.scale + 1, '0');
    const intPart = digits.slice(0, digits.length - this.scale) || '0';
    let frac = this.scale > 0 ? digits.slice(digits.length - this.scale) : '';
    frac = frac.replace(/0+$/, '');
    return `${neg ? '-' : ''}${intPart}${frac.length > 0 ? `.${frac}` : ''}`;
  }

  /** Fixed-decimal rendering (half-up). */
  toFixed(places: number): string {
    if (places < 0) throw new RangeError('toFixed places < 0');
    if (this.scale === places) return this.toStringRaw();
    const shift = this.scale - places;
    if (shift < 0) {
      const scaled = new Dec(this.units * pow10(-shift), places);
      return scaled.toStringRaw();
    }
    const unit = pow10(shift);
    const q = this.units / unit;
    const r = this.units % unit;
    const absR = r < 0n ? -r : r;
    const rounded = absR * 2n >= unit ? q + (this.units < 0n ? -1n : 1n) : q;
    return new Dec(rounded, places).toStringRaw();
  }

  private toStringRaw(): string {
    const neg = this.units < 0n;
    const digits = (neg ? -this.units : this.units).toString().padStart(this.scale + 1, '0');
    const intPart = digits.slice(0, digits.length - this.scale) || '0';
    const frac = this.scale > 0 ? digits.slice(digits.length - this.scale) : '';
    return `${neg ? '-' : ''}${intPart}${frac.length > 0 ? `.${frac}` : ''}`;
  }

  /** Grouped display string: `12,345.67` (en-US separators, en-US only). */
  toDisplay(places?: number): string {
    const s = places === undefined ? this.toString() : this.toFixed(places);
    const neg = s.startsWith('-');
    const body = neg ? s.slice(1) : s;
    const dot = body.indexOf('.');
    const intPart = dot === -1 ? body : body.slice(0, dot);
    const frac = dot === -1 ? '' : body.slice(dot);
    const grouped = intPart.replace(/\B(?=(\d{3})+(?!\d))/g, ',');
    return `${neg ? '-' : ''}${grouped}${frac}`;
  }

  /** Display-only conversion — never use for order math. */
  toNumber(): number {
    return Number(this.toString());
  }
}

/** Convenience alias — `dec('1.23')` / `dec(d)`. */
export function dec(v: string | number | bigint | Dec): Dec {
  return Dec.of(v);
}

/** Lenient alias for wire payloads (`unknown` → `Dec | undefined`). */
export function tryDec(v: unknown): Dec | undefined {
  if (typeof v === 'string' || typeof v === 'number' || typeof v === 'bigint' || v instanceof Dec) {
    return Dec.tryOf(v);
  }
  return undefined;
}

export function minDec(a: Dec, b: Dec): Dec {
  return a.lte(b) ? a : b;
}
export function maxDec(a: Dec, b: Dec): Dec {
  return a.gte(b) ? a : b;
}
