import { describe, expect, it } from 'vitest';

import type { Candle } from '@/lib/candles/klines';

import {
  DEFAULT_COSTS,
  presetById,
  runBacktest,
  smaCrossStrategy,
  type BacktestConfig,
} from './engine';

/** Flat-topped bars: open === close === price at each step. */
function barsAt(prices: number[]): Candle[] {
  return prices.map((p, i) => ({
    openTimeMs: 1_000_000 + i * 3_600_000,
    open: p,
    high: p,
    low: p,
    close: p,
    volume: 0,
    closed: true,
  }));
}

const ZERO_COSTS = { spreadBps: 0, commissionBps: 0, slippageBps: 0, swapBpsPerDay: 0 };

function cfg(over: Partial<BacktestConfig> = {}): BacktestConfig {
  return {
    symbol: 'EUR/USD',
    interval: '1h',
    initialEquity: 10_000,
    positionFraction: 1,
    costs: { ...ZERO_COSTS },
    ...over,
  };
}

/** Signals array: entry target applied at the NEXT bar's open. */
function sig(...vals: number[]): Int8Array {
  return Int8Array.from(vals);
}

describe('runBacktest', () => {
  it('flat signal → no trades, equity unchanged', () => {
    const bars = barsAt([100, 101, 102]);
    const r = runBacktest(bars, sig(0, 0, 0), cfg());
    expect(r.trades).toHaveLength(0);
    expect(r.finalEquity).toBe(10_000);
    expect(r.netPnl).toBe(0);
    expect(r.winRate).toBeNull();
    expect(r.equityCurve).toHaveLength(3);
  });

  it('long signal entered at next-bar open, liquidated at last close', () => {
    // open[1]=101 fill → last close 104 exit; qty = 10000/101.
    const bars = barsAt([100, 101, 102, 103, 104]);
    const r = runBacktest(bars, sig(1, 1, 1, 1, 1), cfg());
    expect(r.trades).toHaveLength(1);
    const t = r.trades[0]!;
    expect(t.direction).toBe(1);
    expect(t.entryPrice).toBeCloseTo(101, 8);
    expect(t.exitPrice).toBeCloseTo(104, 8);
    expect(t.grossPnl).toBeCloseTo(3 * (10_000 / 101), 6);
    expect(r.netPnl).toBeCloseTo(t.grossPnl, 6);
    expect(r.winRate).toBe(1);
    expect(r.maxDrawdownPct).toBe(0);
  });

  it('short position profits on a falling series (sign regression)', () => {
    const bars = barsAt([100, 99, 98, 97, 96]);
    const r = runBacktest(bars, sig(-1, -1, -1, -1, -1), cfg());
    expect(r.trades).toHaveLength(1);
    const t = r.trades[0]!;
    expect(t.direction).toBe(-1);
    // Entry fill = 99, exit = 96, qty = 10000/99 → gross = 3 * qty.
    expect(t.grossPnl).toBeCloseTo(3 * (10_000 / 99), 6);
    expect(r.netPnl).toBeGreaterThan(0);
  });

  it('commission is charged per side and counted in netPnl', () => {
    const bars = barsAt([100, 100, 100, 100, 100]);
    const costs = { ...ZERO_COSTS, commissionBps: 10 }; // 10bps = 0.1%
    const r = runBacktest(bars, sig(1, 1, 1, 1, 1), cfg({ costs }));
    const t = r.trades[0]!;
    // Flat prices ⇒ gross 0; entry comm = 10000*0.001 = 10, exit ~10.
    expect(t.grossPnl).toBeCloseTo(0, 8);
    expect(t.netPnl).toBeLessThan(0);
    expect(r.totalCommission).toBeCloseTo(-t.netPnl, 6);
    expect(r.totalCommission).toBeCloseTo(20, 1);
  });

  it('spread/slippage fills adversely for buys and sells', () => {
    const bars = barsAt([100, 100, 100]);
    const costs = { ...ZERO_COSTS, spreadBps: 100 }; // 1% half-spread
    const r = runBacktest(bars, sig(1, 1, 1), cfg({ costs }));
    const t = r.trades[0]!;
    // Buy fills at open*(1+0.01), sell at open*(1-0.01): 2% round trip.
    expect(t.entryPrice).toBeCloseTo(101, 6);
    expect(t.exitPrice).toBeCloseTo(99, 6);
    expect(r.totalSpreadSlippage).toBeGreaterThan(0);
    expect(r.netPnl).toBeLessThan(0);
  });

  it('swap accrues daily financing while in market', () => {
    const day = 86_400_000;
    const bars: Candle[] = [0, 1, 2].map((i) => ({
      openTimeMs: i * day,
      open: 100,
      high: 100,
      low: 100,
      close: 100,
      volume: 0,
      closed: true,
    }));
    const costs = { ...ZERO_COSTS, swapBpsPerDay: 10 };
    const r = runBacktest(bars, sig(1, 1, 1), cfg({ costs }));
    // ~2 days in market at 10bps/day on ~10000 notional ⇒ ~20 charged.
    expect(r.totalSwap).toBeGreaterThan(0);
    expect(r.totalSwap).toBeCloseTo(20, 0);
    expect(r.netPnl).toBeLessThan(0);
  });

  it('tracks max drawdown on a losing excursion', () => {
    // Enter at bar1 open=100; price falls to 50 then recovers to 90.
    const bars = barsAt([100, 100, 50, 90]);
    const r = runBacktest(bars, sig(1, 1, 1, 1), cfg());
    expect(r.maxDrawdownPct).toBeCloseTo(50, 1);
    expect(r.netPnl).toBeCloseTo(-10 * (10_000 / 100), 4);
    expect(r.winRate).toBe(0);
  });

  it('is deterministic — same inputs, same result', () => {
    const bars = barsAt([100, 101, 99, 103, 98, 105]);
    const signals = sig(1, -1, 1, 0, 1, -1);
    const a = runBacktest(bars, signals, cfg({ costs: { ...DEFAULT_COSTS } }));
    const b = runBacktest(bars, signals, cfg({ costs: { ...DEFAULT_COSTS } }));
    expect(a).toEqual(b);
  });

  it('clamps out-of-range signal values', () => {
    const bars = barsAt([100, 100, 100]);
    const r = runBacktest(bars, sig(7, -3, 0), cfg());
    // 7 → long (bar1 open), -3 → flip short (bar2 open), end-of-run liquidation.
    expect(r.trades).toHaveLength(2);
    expect(r.trades[0]!.direction).toBe(1);
    expect(r.trades[1]!.direction).toBe(-1);
  });
});

describe('strategy presets', () => {
  it('smaCrossStrategy emits a full-length signal array', () => {
    const closes = Array.from({ length: 60 }, (_, i) => 100 + i);
    const bars = closes.map((c, i) => ({
      openTimeMs: i * 60_000,
      open: c,
      high: c,
      low: c,
      close: c,
      volume: 0,
      closed: true,
    }));
    const p = smaCrossStrategy(5, 20);
    const s = p.signals(bars);
    expect(s).toHaveLength(60);
    // Monotonically rising ⇒ fast SMA > slow once defined ⇒ all 1s.
    for (let i = 19; i < 60; i++) expect(s[i]).toBe(1);
  });

  it('presetById returns a labeled preset for each id', () => {
    for (const id of ['sma-cross', 'rsi-mean-revert', 'ema-momentum']) {
      const p = presetById(id);
      expect(p.id).toBe(id);
      expect(p.label.length).toBeGreaterThan(0);
    }
  });
});
