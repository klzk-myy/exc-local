/**
 * Sandboxed, cost-aware backtester (Phase-10 Task 10.3.16).
 *
 * Execution model — no lookahead:
 *   a strategy computes a *target exposure* per bar (-1 short / 0 flat /
 *   +1 long) from data through that bar's close; the fill happens at the
 *   NEXT bar's open, adjusted by half-spread + slippage (bps) against the
 *   taker. Commission is charged per side on notional. Swap/financing is
 *   accrued per bar as swapBpsPerDay × notional × barFraction-of-day.
 *
 * Disclosures (surfaced verbatim in the UI per the task text): spread,
 * commission, swap, slippage assumptions; fills at next-bar open; no
 * partial fills; spot-style symmetric shorting; historical bars are
 * venue klines — no survivorship bias concept applies to FX pairs, and
 * data latency equals the bar close (no tick-level queue modeling).
 */

import type { Candle } from '@/lib/candles/klines';
import { ema, rsi, sma, type IndicatorSeries } from '@/lib/indicators/indicators';

export interface BacktestCosts {
  /** Half-spread in basis points, applied adversely at each fill. */
  spreadBps: number;
  /** Per-side commission in bps of notional. */
  commissionBps: number;
  /** Adverse slippage in bps per fill. */
  slippageBps: number;
  /** Daily financing on open notional (bps/day; 0 disables). */
  swapBpsPerDay: number;
}

export const DEFAULT_COSTS: BacktestCosts = {
  spreadBps: 0.6,
  commissionBps: 0.4,
  slippageBps: 0.2,
  swapBpsPerDay: 0,
};

export interface BacktestConfig {
  symbol: string;
  interval: string;
  /** Starting equity in quote-currency terms. */
  initialEquity: number;
  /** Fraction of equity committed per position (0–1]. */
  positionFraction: number;
  costs: BacktestCosts;
}

export interface BacktestTrade {
  entryIdx: number;
  exitIdx: number;
  direction: 1 | -1;
  entryPrice: number;
  exitPrice: number;
  quantity: number;
  grossPnl: number;
  netPnl: number;
  entryTimeMs: number;
  exitTimeMs: number;
}

export interface EquityPoint {
  timeMs: number;
  equity: number;
}

export interface BacktestResult {
  config: BacktestConfig;
  strategyName: string;
  equityCurve: EquityPoint[];
  trades: BacktestTrade[];
  netPnl: number;
  netPnlPct: number;
  winRate: number | null; // null when no closed trades
  maxDrawdownPct: number;
  totalCommission: number;
  totalSwap: number;
  totalSpreadSlippage: number;
  finalEquity: number;
  barsInMarket: number;
}

/** A strategy maps candles to a per-bar target exposure (-1|0|+1),
 * computed causally (index i sees bars[0..i] only). */
export type StrategySignals = (bars: readonly Candle[]) => Int8Array;

export interface StrategyPreset {
  id: string;
  label: string;
  params: Record<string, number>;
  signals: StrategySignals;
}

const lastDefined = (s: IndicatorSeries, i: number): number | null => s[i] ?? null;

/** SMA crossover: long while SMA(fast) > SMA(slow), short while below. */
export function smaCrossStrategy(fast = 10, slow = 30): StrategyPreset {
  return {
    id: 'sma-cross',
    label: `SMA crossover (${fast}/${slow})`,
    params: { fast, slow },
    signals: (bars) => {
      const closes = bars.map((b) => b.close);
      const f = sma(closes, fast);
      const s = sma(closes, slow);
      const out = new Int8Array(bars.length);
      for (let i = 0; i < bars.length; i++) {
        const fv = lastDefined(f, i);
        const sv = lastDefined(s, i);
        out[i] = fv === null || sv === null ? 0 : fv > sv ? 1 : fv < sv ? -1 : 0;
      }
      return out;
    },
  };
}

/** RSI mean-reversion: long below `lower`, exit at the midline; short
 * above `upper`, exit at the midline. */
export function rsiMeanRevertStrategy(period = 14, lower = 30, upper = 70): StrategyPreset {
  return {
    id: 'rsi-mean-revert',
    label: `RSI mean-reversion (${period}, ${lower}/${upper})`,
    params: { period, lower, upper },
    signals: (bars) => {
      const closes = bars.map((b) => b.close);
      const r = rsi(closes, period);
      const out = new Int8Array(bars.length);
      let pos = 0;
      for (let i = 0; i < bars.length; i++) {
        const v = lastDefined(r, i);
        if (v === null) {
          out[i] = 0;
          continue;
        }
        if (pos === 0) {
          if (v < lower) pos = 1;
          else if (v > upper) pos = -1;
        } else if (pos === 1 && v >= 50) {
          pos = 0;
        } else if (pos === -1 && v <= 50) {
          pos = 0;
        }
        out[i] = pos;
      }
      return out;
    },
  };
}

/** EMA momentum: long while EMA(fast) > EMA(slow), flat otherwise. */
export function emaMomentumStrategy(fast = 12, slow = 48): StrategyPreset {
  return {
    id: 'ema-momentum',
    label: `EMA momentum (${fast}/${slow})`,
    params: { fast, slow },
    signals: (bars) => {
      const closes = bars.map((b) => b.close);
      const f = ema(closes, fast);
      const s = ema(closes, slow);
      const out = new Int8Array(bars.length);
      for (let i = 0; i < bars.length; i++) {
        const fv = lastDefined(f, i);
        const sv = lastDefined(s, i);
        out[i] = fv !== null && sv !== null && fv > sv ? 1 : 0;
      }
      return out;
    },
  };
}

export const STRATEGY_PRESETS: StrategyPreset[] = [
  smaCrossStrategy(),
  rsiMeanRevertStrategy(),
  emaMomentumStrategy(),
];

export function presetById(id: string, params?: Record<string, number>): StrategyPreset {
  switch (id) {
    case 'rsi-mean-revert':
      return rsiMeanRevertStrategy(params?.['period'], params?.['lower'], params?.['upper']);
    case 'ema-momentum':
      return emaMomentumStrategy(params?.['fast'], params?.['slow']);
    case 'sma-cross':
    default:
      return smaCrossStrategy(params?.['fast'], params?.['slow']);
  }
}

const BPS = 1 / 10_000;

/** Run a backtest. Pure, synchronous, deterministic for a given input —
 * results are reproducible per the task's reproducibility checkpoint. */
export function runBacktest(
  bars: readonly Candle[],
  signals: Int8Array,
  config: BacktestConfig,
  strategyName = '',
): BacktestResult {
  const { costs } = config;
  const adverseBps = (costs.spreadBps + costs.slippageBps) * BPS;
  const n = bars.length;
  const equityCurve: EquityPoint[] = [];
  const trades: BacktestTrade[] = [];

  let cash = config.initialEquity;
  let pos = 0; // signed quantity (base units)
  let posDir: 0 | 1 | -1 = 0;
  let entryIdx = -1;
  let entryPrice = 0;
  let entryNotional = 0;
  let entryComm = 0;
  let totalCommission = 0;
  let totalSwap = 0;
  let totalSpreadSlippage = 0;
  let peak = config.initialEquity;
  let maxDd = 0;
  let barsInMarket = 0;

  const commission = (notional: number) => Math.abs(notional) * costs.commissionBps * BPS;

  for (let i = 0; i < n; i++) {
    const bar = bars[i];
    if (bar === undefined) break;
    // Fill pending signal changes at this bar's open.
    if (i > 0) {
      const raw = signals[i - 1] ?? 0;
      const target = raw <= -1 ? -1 : raw >= 1 ? 1 : 0;
      if (target !== posDir) {
        // Close existing.
        if (pos !== 0) {
          const dir: 1 | -1 = posDir === -1 ? -1 : 1;
          const exitPx = bar.open * (dir === 1 ? 1 - adverseBps : 1 + adverseBps);
          // pos is signed — short P&L emerges correctly (entry−exit).
          const gross = (exitPx - entryPrice) * pos;
          const slip = Math.abs(exitPx * pos) * adverseBps;
          const comm = commission(Math.abs(exitPx * pos));
          cash += gross - comm;
          totalCommission += comm;
          totalSpreadSlippage += slip;
          trades.push({
            entryIdx,
            exitIdx: i,
            direction: dir,
            entryPrice,
            exitPrice: exitPx,
            quantity: Math.abs(pos),
            grossPnl: gross,
            netPnl: gross - entryComm - comm,
            entryTimeMs: bars[entryIdx]?.openTimeMs ?? bar.openTimeMs,
            exitTimeMs: bar.openTimeMs,
          });
          pos = 0;
          posDir = 0;
        }
        // Open new.
        if (target !== 0) {
          const notional = cash * config.positionFraction;
          const fillPx = bar.open * (target === 1 ? 1 + adverseBps : 1 - adverseBps);
          const qty = (notional / fillPx) * target;
          const comm = commission(Math.abs(qty * fillPx));
          const slip = Math.abs(qty * fillPx) * adverseBps;
          cash -= comm;
          totalCommission += comm;
          totalSpreadSlippage += slip;
          pos = qty;
          posDir = target;
          entryIdx = i;
          entryPrice = fillPx;
          entryNotional = Math.abs(qty * fillPx);
          entryComm = comm;
        }
      }
    }

    // Swap/financing accrual while in market (per-bar day fraction).
    const prevBar = bars[i - 1];
    if (pos !== 0 && costs.swapBpsPerDay !== 0 && prevBar !== undefined) {
      const dtMs = bar.openTimeMs - prevBar.openTimeMs;
      const charge = entryNotional * costs.swapBpsPerDay * BPS * (dtMs / 86_400_000);
      cash -= charge;
      totalSwap += charge;
    }
    if (pos !== 0) barsInMarket++;

    // Positions are fully funded (cash-secured): equity = cash +
    // unrealized P&L since entry — the notional was never debited.
    const e = pos === 0 ? cash : cash + (bar.close - entryPrice) * pos;
    if (e > peak) peak = e;
    const dd = peak > 0 ? (peak - e) / peak : 0;
    if (dd > maxDd) maxDd = dd;
    equityCurve.push({ timeMs: bar.openTimeMs, equity: e });
  }

  // Liquidate any open position at the last close for a closed-trade tally.
  const lastBar = bars[n - 1];
  if (pos !== 0 && lastBar !== undefined) {
    const dir: 1 | -1 = posDir === -1 ? -1 : 1;
    const exitPx = lastBar.close * (dir === 1 ? 1 - adverseBps : 1 + adverseBps);
    const gross = (exitPx - entryPrice) * pos;
    const comm = commission(Math.abs(exitPx * pos));
    const slip = Math.abs(exitPx * pos) * adverseBps;
    cash += gross - comm;
    totalCommission += comm;
    totalSpreadSlippage += slip;
    trades.push({
      entryIdx,
      exitIdx: n - 1,
      direction: dir,
      entryPrice,
      exitPrice: exitPx,
      quantity: Math.abs(pos),
      grossPnl: gross,
      netPnl: gross - entryComm - comm,
      entryTimeMs: bars[entryIdx]?.openTimeMs ?? lastBar.openTimeMs,
      exitTimeMs: lastBar.openTimeMs,
    });
    equityCurve.push({ timeMs: lastBar.openTimeMs, equity: cash });
    pos = 0;
    posDir = 0;
  }

  const finalEquity = cash;
  const netPnl = finalEquity - config.initialEquity;
  const wins = trades.filter((t) => t.netPnl > 0).length;

  return {
    config,
    strategyName,
    equityCurve,
    trades,
    netPnl,
    netPnlPct: config.initialEquity !== 0 ? (netPnl / config.initialEquity) * 100 : 0,
    winRate: trades.length > 0 ? wins / trades.length : null,
    maxDrawdownPct: maxDd * 100,
    totalCommission,
    totalSwap,
    totalSpreadSlippage,
    finalEquity,
    barsInMarket,
  };
}
