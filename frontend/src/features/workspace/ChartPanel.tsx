/**
 * Chart panel — timeframe switcher + TradingChart (Task 10.3.9 chart
 * tile). The interval is panel state persisted under the same exc.*
 * localStorage convention as layouts/watchlists so remounts (layout
 * edits, mode switches) keep the trader's timeframe. Switching re-keys
 * the chart — TradingChart's useLwcChart deps already recreate the
 * canvas and resubscribe `kline@{symbol}_{interval}`.
 */
import { lazy, useState } from 'react';

import {
  KLINE_INTERVAL_SET,
  type KlineInterval,
} from '@/lib/market/channels';

const TradingChart = lazy(() => import('@/features/charts/TradingChart'));

/** The strip subset — the canonical 13-frame set stays reachable via
 * the full chart page; the tile offers the common trading frames. */
const INTERVALS: readonly KlineInterval[] = ['1m', '5m', '15m', '1h', '4h', '1D'];

const STORAGE_KEY = 'exc.chart.interval.v1';

function loadInterval(): KlineInterval {
  try {
    const v = window.localStorage.getItem(STORAGE_KEY);
    if (v !== null && KLINE_INTERVAL_SET.has(v)) return v as KlineInterval;
  } catch {
    /* storage blocked — fall through to default */
  }
  return '15m';
}

export function ChartPanel({ symbol }: { symbol: string }) {
  const [interval, setInterval] = useState<KlineInterval>(loadInterval);
  const pick = (i: KlineInterval) => {
    setInterval(i);
    try {
      window.localStorage.setItem(STORAGE_KEY, i);
    } catch {
      /* non-fatal — session state still applies */
    }
  };
  return (
    <div className="space-y-1">
      <div className="flex gap-1" role="group" aria-label="Chart interval">
        {INTERVALS.map((i) => (
          <button
            key={i}
            type="button"
            aria-pressed={interval === i}
            onClick={() => pick(i)}
            className={`rounded px-2.5 py-1.5 font-mono text-xs ${
              interval === i
                ? 'bg-sky-600 text-white'
                : 'text-neutral-400 hover:bg-neutral-800 hover:text-neutral-200'
            }`}
          >
            {i}
          </button>
        ))}
      </div>
      <TradingChart symbol={symbol} interval={interval} />
    </div>
  );
}
