import { describe, expect, it } from 'vitest';

import { bollinger, ema, macd, rsi, sma, vwap, wma, type OhlcvBar } from './indicators';

const approx = (v: number | null | undefined, expected: number, dp = 4) => {
  expect(v).not.toBeNull();
  expect(v!).toBeCloseTo(expected, dp);
};

describe('sma', () => {
  it('computes the rolling mean with null warm-up', () => {
    const out = sma([1, 2, 3, 4, 5, 6, 7, 8, 9, 10], 3);
    expect(out).toEqual([null, null, 2, 3, 4, 5, 6, 7, 8, 9]);
  });
  it('returns all-null when the series is shorter than the period', () => {
    expect(sma([1, 2], 5)).toEqual([null, null]);
  });
  it('rejects non-positive periods', () => {
    expect(() => sma([1], 0)).toThrow();
  });
});

describe('ema', () => {
  it('seeds on the SMA of the first period then smooths', () => {
    const out = ema([1, 2, 3, 4, 5], 3);
    // seed = 2 @ idx2; idx3 = 4*.5+2*.5 = 3; idx4 = 5*.5+3*.5 = 4
    expect(out).toEqual([null, null, 2, 3, 4]);
  });
});

describe('wma', () => {
  it('weights newest values linearly', () => {
    const out = wma([1, 2, 3, 4, 5], 3);
    approx(out[2], 14 / 6);
    approx(out[3], 20 / 6);
    approx(out[4], 26 / 6);
  });
});

describe('rsi', () => {
  // Canonical Wilder/StockCharts worked example.
  const closes = [
    44.34, 44.09, 44.15, 43.61, 44.33, 44.83, 45.1, 45.42, 45.84, 46.08, 45.89, 46.03, 45.61, 46.28,
    46.28, 46.0,
  ];
  it('matches the reference values for the canonical series', () => {
    const out = rsi(closes, 14);
    approx(out[14], 70.46, 2);
    approx(out[15], 66.25, 2);
  });
  it('reports 100 when there are no losses', () => {
    const out = rsi([1, 2, 3, 4, 5, 6], 3);
    expect(out[3]).toBe(100);
    expect(out[5]).toBe(100);
  });
  it('reports ~0 on a strictly-falling series', () => {
    const out = rsi([10, 9, 8, 7, 6, 5], 3);
    approx(out[5], 0, 6);
  });
});

describe('macd', () => {
  it('is zero on a constant series', () => {
    const out = macd(new Array(40).fill(7) as number[]);
    expect(out[39]!.macd).toBe(0);
    expect(out[39]!.signal).toBe(0);
    expect(out[39]!.histogram).toBe(0);
  });
  it('histogram equals macd − signal when both defined', () => {
    const values = Array.from({ length: 60 }, (_, i) => 100 + Math.sin(i / 4) * 5 + i * 0.1);
    const out = macd(values);
    const last = out[59]!;
    approx(last.histogram, last.macd! - last.signal!, 8);
  });
  it('is null during the warm-up window', () => {
    const out = macd([1, 2, 3, 4, 5], 2, 3, 2);
    expect(out[0]!.macd).toBeNull();
    expect(out[1]!.macd).toBeNull();
  });
});

describe('bollinger', () => {
  it('computes SMA ± 2σ', () => {
    const values = Array.from({ length: 20 }, (_, i) => i + 1);
    const out = bollinger(values, 20, 2);
    const p = out[19]!;
    approx(p.middle, 10.5);
    const sd = Math.sqrt(33.25); // population variance of 1..20
    approx(p.upper, 10.5 + 2 * sd);
    approx(p.lower, 10.5 - 2 * sd);
  });
  it('collapses to the mean on a constant series', () => {
    const out = bollinger(new Array(25).fill(3.5) as number[], 20);
    expect(out[24]!.upper).toBeCloseTo(3.5);
    expect(out[24]!.lower).toBeCloseTo(3.5);
  });
});

describe('vwap', () => {
  const bar = (day: number, h: number, l: number, c: number, v: number): OhlcvBar => ({
    openTimeMs: Date.UTC(2026, 0, day, 12, 0, 0),
    open: c,
    high: h,
    low: l,
    close: c,
    volume: v,
  });

  it('accumulates typical·volume / volume', () => {
    const out = vwap([bar(5, 2, 1, 1.5, 10), bar(5, 3, 2, 2.5, 10)]);
    approx(out[0], 1.5);
    approx(out[1], 2.0);
  });
  it('resets at the session (UTC day) boundary by default', () => {
    const out = vwap([bar(5, 2, 1, 1.5, 10), bar(6, 100, 100, 100, 10)]);
    approx(out[1], 100);
  });
});
