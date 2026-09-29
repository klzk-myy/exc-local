/**
 * Fixed-point decimal for UI-layer financial math (spec §5.3: no float
 * money). Values arrive from the wire as decimal strings; `Dec` keeps
 * them exact for aggregation (cumulative depth, spread, notional) so the
 * book/order-entry/portfolio surfaces never accumulate float error.
 *
 * This is a deliberately small surface: parse / add / sub / mul / divInt /
 * compare / format. Anything richer belongs in the Go services.
 */

const DEC_RE = /^([+-]?)(\d+)(?:\.(\d+))?$/;

export class Dec {
  /** Integer units at `scale` decimal places (value = units / 10^scale). */
  private constructor(
    readonly units: bigint,
    readonly scale: number,
  ) {}

  static readonly ZERO = new Dec(0n, 0);
  static readonly ONE = new Dec(1n, 0);

  /** Parse a canonical decimal string ("1.23", "-0.5", "100"). Returns
   * null on malformed input — callers fail closed. */
  static parse(s: string): Dec | null {
    const m = DEC_RE.exec(s.trim());
    if (!m) return null;
    const frac = m[3] ?? '';
    const units = BigInt(`${m[1] === '-' ? '-' : ''}${m[2]}${frac}`);
    return new Dec(units, frac.length);
  }

  /** Non-null parse for literal constants in code/tests. */
  static of(s: string): Dec {
    const d = Dec.parse(s);
    if (d === null) throw new Error(`Dec.of: malformed decimal ${s}`);
    return d;
  }

  static fromInt(n: number | bigint): Dec {
    return new Dec(BigInt(n), 0);
  }

  private align(o: Dec): [bigint, bigint, number] {
    const scale = Math.max(this.scale, o.scale);
    return [
      this.units * 10n ** BigInt(scale - this.scale),
      o.units * 10n ** BigInt(scale - o.scale),
      scale,
    ];
  }

  add(o: Dec): Dec {
    const [a, b, scale] = this.align(o);
    return new Dec(a + b, scale);
  }

  sub(o: Dec): Dec {
    const [a, b, scale] = this.align(o);
    return new Dec(a - b, scale);
  }

  neg(): Dec {
    return new Dec(-this.units, this.scale);
  }

  /** Exact multiplication (scales add — callers normalize for display). */
  mul(o: Dec): Dec {
    return new Dec(this.units * o.units, this.scale + o.scale);
  }

  mulInt(n: number | bigint): Dec {
    return new Dec(this.units * BigInt(n), this.scale);
  }

  /** Integer division truncated toward zero. `extraScale` raises the
   * result precision before dividing — mid = (bid+ask).divInt(2, 1)
   * keeps the half-tick so display rounding lands correctly. */
  divInt(n: number | bigint, extraScale = 0): Dec {
    if (extraScale === 0) return new Dec(this.units / BigInt(n), this.scale);
    const scaled = this.units * 10n ** BigInt(extraScale);
    return new Dec(scaled / BigInt(n), this.scale + extraScale);
  }

  cmp(o: Dec): number {
    const [a, b] = this.align(o);
    return a < b ? -1 : a > b ? 1 : 0;
  }

  eq(o: Dec): boolean {
    return this.cmp(o) === 0;
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

  isZero(): boolean {
    return this.units === 0n;
  }
  isPositive(): boolean {
    return this.units > 0n;
  }
  isNegative(): boolean {
    return this.units < 0n;
  }

  /** Integer multiple check — lot/tick alignment (`qty mod step == 0`). */
  isMultipleOf(step: Dec): boolean {
    if (step.isZero()) return false;
    const scale = Math.max(this.scale, step.scale);
    const a = this.units * 10n ** BigInt(scale - this.scale);
    const b = step.units * 10n ** BigInt(scale - step.scale);
    return b !== 0n && a % b === 0n;
  }

  /** Round-half-up to `places` fraction digits, rendered string. */
  toFixed(places: number): string {
    if (places < 0) throw new Error('Dec.toFixed: places must be >= 0');
    const neg = this.units < 0n;
    const abs = neg ? -this.units : this.units;
    let scaled: bigint;
    if (this.scale > places) {
      const drop = this.scale - places;
      const div = 10n ** BigInt(drop);
      const half = div / 2n;
      scaled = (abs + half) / div;
    } else {
      scaled = abs * 10n ** BigInt(places - this.scale);
    }
    const s = scaled.toString().padStart(places + 1, '0');
    const int = places === 0 ? s : s.slice(0, -places);
    const frac = places === 0 ? '' : `.${s.slice(-places)}`;
    return `${neg && scaled !== 0n ? '-' : ''}${int}${frac}`;
  }

  /** Canonical decimal string (trailing fraction zeros trimmed). */
  toString(): string {
    const neg = this.units < 0n;
    const abs = (neg ? -this.units : this.units).toString();
    if (this.scale === 0) return `${neg ? '-' : ''}${abs}`;
    const s = abs.padStart(this.scale + 1, '0');
    const int = s.slice(0, -this.scale);
    const frac = s.slice(-this.scale).replace(/0+$/, '');
    return `${neg ? '-' : ''}${int}${frac === '' ? '' : `.${frac}`}`;
  }

  /** Float approximation — DISPLAY ONLY (depth bar ratios, chart series).
   * Never feed back into order parameters. */
  toNumber(): number {
    return Number(this.units) / 10 ** this.scale;
  }
}

/** Parse-or-zero for optional wire fields (never throws at the edge —
 * malformed numerics are the parser's problem, surfaces show 0). */
export function decOrZero(s: string | null | undefined): Dec {
  if (s === null || s === undefined) return Dec.ZERO;
  return Dec.parse(s) ?? Dec.ZERO;
}

/** Decimal places implied by a step size ("0.0001" → 4, "0.5" → 1,
 * "1000" → 0). Malformed/integer-only steps return 0. */
export function stepDecimals(step: string): number {
  const d = Dec.parse(step);
  return d === null ? 0 : d.scale;
}
