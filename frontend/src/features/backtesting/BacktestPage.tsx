/**
 * Backtesting workbench (Phase-10 Task 10.3.16) — indicator presets +
 * cost-aware sandboxed engine over historical klines.
 *
 * Flow: pick a symbol/interval/window → lazy chunked fetch via
 * lib/candles/fetchKlines (cursor-paged) → strategy preset computes
 * causal per-bar signals → lib/backtest/runBacktest fills at the NEXT
 * bar's open with spread/slippage/commission/swap costs → results panel
 * shows net P&L, win rate, max drawdown, the equity curve (inline SVG —
 * deterministic, no canvas), trade list, and the assumption disclosures
 * the task requires verbatim.
 */
import { useState } from 'react';

import { apiClient } from '@/app/runtime';
import type { ApiClient } from '@/lib/api';
import { fetchKlines, KLINE_INTERVALS, type Candle } from '@/lib/candles/klines';
import {
  DEFAULT_COSTS,
  presetById,
  runBacktest,
  STRATEGY_PRESETS,
  type BacktestResult,
} from '@/lib/backtest/engine';
import {
  btnPrimary,
  cardCls,
  inputCls,
  labelCls,
  tableCls,
  tdCls,
  thCls,
  ErrorBox,
} from '@/lib/ui';

const RANGES = [
  { label: '7 days', ms: 7 * 86_400_000 },
  { label: '30 days', ms: 30 * 86_400_000 },
  { label: '90 days', ms: 90 * 86_400_000 },
] as const;

interface ParamSpec {
  key: string;
  label: string;
  def: number;
}

const PARAM_SPECS: Record<string, ParamSpec[]> = {
  'sma-cross': [
    { key: 'fast', label: 'Fast SMA', def: 10 },
    { key: 'slow', label: 'Slow SMA', def: 30 },
  ],
  'rsi-mean-revert': [
    { key: 'period', label: 'RSI period', def: 14 },
    { key: 'lower', label: 'Lower band', def: 30 },
    { key: 'upper', label: 'Upper band', def: 70 },
  ],
  'ema-momentum': [
    { key: 'fast', label: 'Fast EMA', def: 12 },
    { key: 'slow', label: 'Slow EMA', def: 48 },
  ],
};

/** Equity curve as an inline SVG polyline — display-only float math is
 * fine here (§5.3 applies to money on the wire, not chart pixels). */
function EquityCurve({ result }: { result: BacktestResult }) {
  const pts = result.equityCurve;
  if (pts.length < 2) return <p className="text-xs text-neutral-500">Not enough bars to chart.</p>;
  const w = 640;
  const h = 160;
  const min = Math.min(...pts.map((p) => p.equity));
  const max = Math.max(...pts.map((p) => p.equity));
  const span = max - min || 1;
  const t0 = pts[0]?.timeMs ?? 0;
  const t1 = pts[pts.length - 1]?.timeMs ?? 1;
  const tspan = t1 - t0 || 1;
  const polyline = pts
    .map(
      (p) =>
        `${(((p.timeMs - t0) / tspan) * w).toFixed(1)},${(h - ((p.equity - min) / span) * h).toFixed(1)}`,
    )
    .join(' ');
  const up = (pts[pts.length - 1]?.equity ?? 0) >= result.config.initialEquity;
  return (
    <svg
      viewBox={`0 0 ${w} ${h}`}
      className="h-40 w-full"
      role="img"
      aria-label="Equity curve"
      data-testid="equity-curve"
    >
      <line
        x1="0"
        y1={h - ((result.config.initialEquity - min) / span) * h}
        x2={w}
        y2={h - ((result.config.initialEquity - min) / span) * h}
        stroke="#404040"
        strokeDasharray="4"
      />
      <polyline
        fill="none"
        stroke={up ? '#34d399' : '#f87171'}
        strokeWidth="1.5"
        points={polyline}
      />
    </svg>
  );
}

function num(v: string, fallback: number): number {
  if (v.trim() === '') return fallback;
  const n = Number(v);
  return Number.isFinite(n) ? n : fallback;
}

export default function BacktestPage({ api = apiClient }: { api?: ApiClient }) {
  const [symbol, setSymbol] = useState('EUR/USD');
  const [interval, setInterval] = useState('1h');
  const [rangeMs, setRangeMs] = useState<number>(RANGES[1].ms);
  const [presetId, setPresetId] = useState('sma-cross');
  const [params, setParams] = useState<Record<string, string>>({});
  const [equity, setEquity] = useState('10000');
  const [fraction, setFraction] = useState('1');
  const [spreadBps, setSpreadBps] = useState(String(DEFAULT_COSTS.spreadBps));
  const [commissionBps, setCommissionBps] = useState(String(DEFAULT_COSTS.commissionBps));
  const [slippageBps, setSlippageBps] = useState(String(DEFAULT_COSTS.slippageBps));
  const [swapBpsPerDay, setSwapBpsPerDay] = useState(String(DEFAULT_COSTS.swapBpsPerDay));

  const [busy, setBusy] = useState(false);
  const [progress, setProgress] = useState('');
  const [error, setError] = useState<unknown>(null);
  const [result, setResult] = useState<BacktestResult | null>(null);
  const [barCount, setBarCount] = useState(0);

  const run = async () => {
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      const toMs = Date.now();
      setProgress('Fetching candles…');
      const bars: Candle[] = await fetchKlines(api, symbol, {
        interval,
        fromMs: toMs - rangeMs,
        toMs,
        limit: 1500,
        maxPages: 8,
      });
      setBarCount(bars.length);
      if (bars.length < 30) {
        setError(
          new Error(
            `Only ${bars.length} candles in range — need at least ~30 to warm up indicators.`,
          ),
        );
        return;
      }
      setProgress('Running backtest…');
      const numericParams: Record<string, number> = {};
      for (const spec of PARAM_SPECS[presetId] ?? []) {
        numericParams[spec.key] = num(params[spec.key] ?? '', spec.def);
      }
      const preset = presetById(presetId, numericParams);
      const signals = preset.signals(bars);
      const res = runBacktest(
        bars,
        signals,
        {
          symbol,
          interval,
          initialEquity: num(equity, 10_000),
          positionFraction: Math.min(1, Math.max(0.01, num(fraction, 1))),
          costs: {
            spreadBps: num(spreadBps, 0),
            commissionBps: num(commissionBps, 0),
            slippageBps: num(slippageBps, 0),
            swapBpsPerDay: num(swapBpsPerDay, 0),
          },
        },
        preset.label,
      );
      setResult(res);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
      setProgress('');
    }
  };

  const paramSpecs = PARAM_SPECS[presetId] ?? [];

  return (
    <div className="mx-auto max-w-6xl p-6">
      <h1 className="mb-4 text-2xl font-semibold">Strategy backtester</h1>
      <form
        className={`${cardCls} mb-4 grid grid-cols-2 gap-3 md:grid-cols-4`}
        onSubmit={(e) => {
          e.preventDefault();
          void run();
        }}
      >
        <div>
          <label className={labelCls} htmlFor="bt-symbol">
            Symbol
          </label>
          <input
            id="bt-symbol"
            className={inputCls}
            value={symbol}
            onChange={(e) => setSymbol(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="bt-interval">
            Interval
          </label>
          <select
            id="bt-interval"
            className={inputCls}
            value={interval}
            onChange={(e) => setInterval(e.target.value)}
          >
            {KLINE_INTERVALS.map((i) => (
              <option key={i} value={i}>
                {i}
              </option>
            ))}
          </select>
        </div>
        <div>
          <label className={labelCls} htmlFor="bt-range">
            Window
          </label>
          <select
            id="bt-range"
            className={inputCls}
            value={rangeMs}
            onChange={(e) => setRangeMs(Number(e.target.value))}
          >
            {RANGES.map((r) => (
              <option key={r.ms} value={r.ms}>
                {r.label}
              </option>
            ))}
          </select>
        </div>
        <div>
          <label className={labelCls} htmlFor="bt-preset">
            Strategy
          </label>
          <select
            id="bt-preset"
            className={inputCls}
            value={presetId}
            onChange={(e) => {
              setPresetId(e.target.value);
              setParams({});
            }}
          >
            {STRATEGY_PRESETS.map((p) => (
              <option key={p.id} value={p.id}>
                {p.label}
              </option>
            ))}
          </select>
        </div>
        {paramSpecs.map((spec) => (
          <div key={spec.key}>
            <label className={labelCls} htmlFor={`bt-p-${spec.key}`}>
              {spec.label}
            </label>
            <input
              id={`bt-p-${spec.key}`}
              className={inputCls}
              value={params[spec.key] ?? String(spec.def)}
              onChange={(e) => setParams({ ...params, [spec.key]: e.target.value })}
            />
          </div>
        ))}
        <div>
          <label className={labelCls} htmlFor="bt-equity">
            Initial equity
          </label>
          <input
            id="bt-equity"
            className={inputCls}
            value={equity}
            onChange={(e) => setEquity(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="bt-frac">
            Position fraction
          </label>
          <input
            id="bt-frac"
            className={inputCls}
            value={fraction}
            onChange={(e) => setFraction(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="bt-spread">
            Spread (bps, half)
          </label>
          <input
            id="bt-spread"
            className={inputCls}
            value={spreadBps}
            onChange={(e) => setSpreadBps(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="bt-comm">
            Commission (bps/side)
          </label>
          <input
            id="bt-comm"
            className={inputCls}
            value={commissionBps}
            onChange={(e) => setCommissionBps(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="bt-slip">
            Slippage (bps)
          </label>
          <input
            id="bt-slip"
            className={inputCls}
            value={slippageBps}
            onChange={(e) => setSlippageBps(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="bt-swap">
            Swap (bps/day)
          </label>
          <input
            id="bt-swap"
            className={inputCls}
            value={swapBpsPerDay}
            onChange={(e) => setSwapBpsPerDay(e.target.value)}
          />
        </div>
        <div className="flex items-end">
          <button type="submit" className={btnPrimary} disabled={busy}>
            {busy ? 'Running…' : 'Run backtest'}
          </button>
        </div>
      </form>

      {progress !== '' && (
        <p className="mb-2 text-sm text-neutral-400" role="status">
          {progress}
        </p>
      )}
      <ErrorBox error={error} />

      {result !== null && (
        <div className="space-y-4" data-testid="backtest-result">
          <section className={cardCls}>
            <h2 className="mb-2 text-sm font-medium text-neutral-400">
              {result.strategyName} — {result.config.symbol} {result.config.interval} ({barCount}{' '}
              bars)
            </h2>
            <div className="grid grid-cols-2 gap-3 md:grid-cols-6">
              <Stat
                label="Net P&L"
                value={`${result.netPnl >= 0 ? '+' : ''}${result.netPnl.toFixed(2)} (${result.netPnlPct.toFixed(2)}%)`}
                tone={result.netPnl >= 0 ? 'up' : 'down'}
              />
              <Stat label="Final equity" value={result.finalEquity.toFixed(2)} />
              <Stat
                label="Win rate"
                value={result.winRate === null ? '—' : `${(result.winRate * 100).toFixed(1)}%`}
              />
              <Stat
                label="Max drawdown"
                value={`${result.maxDrawdownPct.toFixed(2)}%`}
                tone="down"
              />
              <Stat label="Trades" value={String(result.trades.length)} />
              <Stat label="Time in market" value={`${result.barsInMarket} bars`} />
            </div>
            <div className="mt-3">
              <EquityCurve result={result} />
            </div>
            <p className="mt-2 text-xs text-neutral-500">
              Costs: commission {result.totalCommission.toFixed(2)} · swap{' '}
              {result.totalSwap.toFixed(2)} · spread/slippage drag ≈{' '}
              {result.totalSpreadSlippage.toFixed(2)} (in quote terms).
            </p>
          </section>

          <section className={cardCls}>
            <h2 className="mb-2 text-sm font-medium text-neutral-400">Trades</h2>
            <table className={tableCls}>
              <thead>
                <tr>
                  <th className={thCls}>Entry</th>
                  <th className={thCls}>Exit</th>
                  <th className={thCls}>Dir</th>
                  <th className={thCls}>Qty</th>
                  <th className={thCls}>Net P&L</th>
                </tr>
              </thead>
              <tbody>
                {result.trades.slice(0, 100).map((t) => (
                  <tr key={`${t.entryIdx}-${t.exitIdx}`}>
                    <td className={tdCls}>
                      {new Date(t.entryTimeMs).toISOString().slice(0, 16)} @{' '}
                      {t.entryPrice.toFixed(5)}
                    </td>
                    <td className={tdCls}>
                      {new Date(t.exitTimeMs).toISOString().slice(0, 16)} @ {t.exitPrice.toFixed(5)}
                    </td>
                    <td className={tdCls}>{t.direction === 1 ? 'LONG' : 'SHORT'}</td>
                    <td className={tdCls}>{t.quantity.toFixed(2)}</td>
                    <td
                      className={`${tdCls} ${t.netPnl >= 0 ? 'text-emerald-400' : 'text-red-400'}`}
                    >
                      {t.netPnl.toFixed(2)}
                    </td>
                  </tr>
                ))}
                {result.trades.length === 0 && (
                  <tr>
                    <td className={tdCls} colSpan={5}>
                      No closed trades.
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
            {result.trades.length > 100 && (
              <p className="mt-1 text-xs text-neutral-500">
                Showing first 100 of {result.trades.length}.
              </p>
            )}
          </section>

          <section className={cardCls}>
            <h2 className="mb-2 text-sm font-medium text-neutral-400">
              Assumptions &amp; disclosures
            </h2>
            <ul className="list-disc space-y-1 pl-5 text-xs text-neutral-500">
              <li>Fills occur at the next bar's open (no lookahead); no partial fills.</li>
              <li>
                Half-spread {result.config.costs.spreadBps}bps + slippage{' '}
                {result.config.costs.slippageBps}bps applied adversely per fill; commission{' '}
                {result.config.costs.commissionBps}bps per side on notional.
              </li>
              <li>
                Swap/financing accrues at {result.config.costs.swapBpsPerDay}bps/day on open
                notional.
              </li>
              <li>
                Historical bars are venue klines for {result.config.symbol}; data granularity is
                bar-close (no tick-level queue modeling). Spot-style symmetric shorting, fully
                cash-funded positions.
              </li>
              <li>
                Deterministic and reproducible: identical inputs (candles, strategy, costs) produce
                identical output.
              </li>
            </ul>
          </section>
        </div>
      )}
    </div>
  );
}

function Stat({ label, value, tone }: { label: string; value: string; tone?: 'up' | 'down' }) {
  const cls =
    tone === 'up' ? 'text-emerald-400' : tone === 'down' ? 'text-red-400' : 'text-neutral-100';
  return (
    <div>
      <p className="text-xs text-neutral-500">{label}</p>
      <p className={`text-lg font-semibold ${cls}`}>{value}</p>
    </div>
  );
}
