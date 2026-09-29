/**
 * Technical-indicator library (Phase-10 Task 10.3.16) — pure functions,
 * no side effects, no `any`. Every function returns a series aligned to
 * the input length with leading `null` warm-up slots, so callers can zip
 * indicators onto candles index-for-index.
 *
 * Conventions:
 *   - Inputs are close-price series (number[]) or OHLC candles for VWAP.
 *   - `null` marks the warm-up window where the indicator is undefined.
 *   - All implementations are the textbook/standard forms: Wilder
 *     smoothing for RSI, EMA12/26 + signal-9 for MACD, ±2σ Bollinger.
 */

export type IndicatorPoint = number | null;
export type IndicatorSeries = IndicatorPoint[];

/** Dense-array element access under noUncheckedIndexedAccess — the
 * bounded loops below provably stay in range; a violation is a bug and
 * throws rather than silently returning undefined. */
function at(values: readonly number[], i: number): number {
  const v = values[i];
  if (v === undefined) throw new RangeError(`indicator index out of range: ${i}`);
  return v;
}

/** Simple Moving Average over `period`. */
export function sma(values: readonly number[], period: number): IndicatorSeries {
  if (period <= 0) throw new Error('sma: period must be > 0');
  const out: IndicatorSeries = new Array<IndicatorPoint>(values.length).fill(null);
  let acc = 0;
  for (let i = 0; i < values.length; i++) {
    acc += at(values, i);
    if (i >= period) acc -= at(values, i - period);
    if (i >= period - 1) out[i] = acc / period;
  }
  return out;
}

/** Exponential Moving Average — seeded with the SMA of the first
 * `period` values, then smoothed with k = 2/(period+1). */
export function ema(values: readonly number[], period: number): IndicatorSeries {
  if (period <= 0) throw new Error('ema: period must be > 0');
  const out: IndicatorSeries = new Array<IndicatorPoint>(values.length).fill(null);
  if (values.length < period) return out;
  const k = 2 / (period + 1);
  let seed = 0;
  for (let i = 0; i < period; i++) seed += at(values, i);
  let prev = seed / period;
  out[period - 1] = prev;
  for (let i = period; i < values.length; i++) {
    prev = at(values, i) * k + prev * (1 - k);
    out[i] = prev;
  }
  return out;
}

/** Weighted Moving Average — linear weights (newest = period). */
export function wma(values: readonly number[], period: number): IndicatorSeries {
  if (period <= 0) throw new Error('wma: period must be > 0');
  const out: IndicatorSeries = new Array<IndicatorPoint>(values.length).fill(null);
  const denom = (period * (period + 1)) / 2;
  for (let i = period - 1; i < values.length; i++) {
    let acc = 0;
    for (let j = 0; j < period; j++) {
      acc += at(values, i - j) * (period - j);
    }
    out[i] = acc / denom;
  }
  return out;
}

/**
 * RSI — Wilder's smoothing. First average gain/loss is the simple mean
 * of the first `period` deltas; afterwards avg = (prev*(p-1) + cur)/p.
 * When avgLoss is 0 the RSI is 100 (no losses ⇒ maximal strength).
 */
export function rsi(values: readonly number[], period = 14): IndicatorSeries {
  if (period <= 0) throw new Error('rsi: period must be > 0');
  const out: IndicatorSeries = new Array<IndicatorPoint>(values.length).fill(null);
  if (values.length <= period) return out;
  let gain = 0;
  let loss = 0;
  for (let i = 1; i <= period; i++) {
    const d = at(values, i) - at(values, i - 1);
    if (d > 0) gain += d;
    else loss -= d;
  }
  let avgGain = gain / period;
  let avgLoss = loss / period;
  out[period] = avgLoss === 0 ? 100 : 100 - 100 / (1 + avgGain / avgLoss);
  for (let i = period + 1; i < values.length; i++) {
    const d = at(values, i) - at(values, i - 1);
    const g = d > 0 ? d : 0;
    const l = d < 0 ? -d : 0;
    avgGain = (avgGain * (period - 1) + g) / period;
    avgLoss = (avgLoss * (period - 1) + l) / period;
    out[i] = avgLoss === 0 ? 100 : 100 - 100 / (1 + avgGain / avgLoss);
  }
  return out;
}

export interface MacdPoint {
  macd: IndicatorPoint;
  signal: IndicatorPoint;
  histogram: IndicatorPoint;
}

const EMPTY_MACD: MacdPoint = { macd: null, signal: null, histogram: null };

/** MACD — EMA(fast) − EMA(slow); signal = EMA(macd, signalPeriod). */
export function macd(
  values: readonly number[],
  fast = 12,
  slow = 26,
  signalPeriod = 9,
): MacdPoint[] {
  const fastEma = ema(values, fast);
  const slowEma = ema(values, slow);
  const out: MacdPoint[] = Array.from({ length: values.length }, () => ({ ...EMPTY_MACD }));
  // The macd line is defined from index slow-1 on (slow EMA warm-up);
  // the signal line is an EMA of that defined prefix, seeded by the SMA
  // of its first signalPeriod values.
  const firstIdx = Math.max(slow - 1, 0);
  const defined: number[] = [];
  for (let i = firstIdx; i < values.length; i++) {
    const f = fastEma[i];
    const s = slowEma[i];
    if (f !== null && f !== undefined && s !== null && s !== undefined) {
      defined.push(f - s);
    }
  }
  const signalLine = ema(defined, signalPeriod);
  for (let i = 0; i < defined.length; i++) {
    const m = defined[i];
    if (m === undefined) continue;
    const s = signalLine[i] ?? null;
    out[i + firstIdx] = {
      macd: m,
      signal: s,
      histogram: s === null ? null : m - s,
    };
  }
  return out;
}

export interface BollingerPoint {
  middle: IndicatorPoint;
  upper: IndicatorPoint;
  lower: IndicatorPoint;
}

const EMPTY_BB: BollingerPoint = { middle: null, upper: null, lower: null };

/** Bollinger Bands — SMA(period) ± deviations·σ (population σ). */
export function bollinger(
  values: readonly number[],
  period = 20,
  deviations = 2,
): BollingerPoint[] {
  if (period <= 0) throw new Error('bollinger: period must be > 0');
  const mid = sma(values, period);
  const out: BollingerPoint[] = Array.from({ length: values.length }, () => ({ ...EMPTY_BB }));
  for (let i = period - 1; i < values.length; i++) {
    const m = mid[i];
    if (m === null || m === undefined) continue;
    let acc = 0;
    for (let j = i - period + 1; j <= i; j++) {
      const d = at(values, j) - m;
      acc += d * d;
    }
    const sd = Math.sqrt(acc / period);
    out[i] = { middle: m, upper: m + deviations * sd, lower: m - deviations * sd };
  }
  return out;
}

export interface OhlcvBar {
  openTimeMs: number;
  open: number;
  high: number;
  low: number;
  close: number;
  volume: number;
}

/** VWAP — cumulative Σ(typical·vol)/Σvol. `isNewSession` restarts the
 * accumulation at a day boundary (FX desks treat VWAP as a daily-anchored
 * statistic — the default detector resets on the UTC day change; pass a
 * custom detector for the venue's 22:00 UTC rollover). */
export function vwap(
  bars: readonly OhlcvBar[],
  opts: { isNewSession?: (prev: OhlcvBar, cur: OhlcvBar) => boolean } = {},
): IndicatorSeries {
  const isNewSession =
    opts.isNewSession ??
    ((prev: OhlcvBar, cur: OhlcvBar) =>
      new Date(prev.openTimeMs).getUTCDay() !== new Date(cur.openTimeMs).getUTCDay());
  const out: IndicatorSeries = new Array<IndicatorPoint>(bars.length).fill(null);
  let pv = 0;
  let vv = 0;
  for (let i = 0; i < bars.length; i++) {
    const b = bars[i];
    if (b === undefined) break;
    const prev = bars[i - 1];
    if (prev !== undefined && isNewSession(prev, b)) {
      pv = 0;
      vv = 0;
    }
    const typical = (b.high + b.low + b.close) / 3;
    pv += typical * b.volume;
    vv += b.volume;
    out[i] = vv > 0 ? pv / vv : typical;
  }
  return out;
}
