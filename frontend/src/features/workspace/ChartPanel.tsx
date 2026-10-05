/**
 * Chart panel — timeframe switcher + TradingChart (Task 10.3.9 chart
 * tile). The interval is panel state persisted under the same exc.*
 * localStorage convention as layouts/watchlists so remounts (layout
 * edits, mode switches) keep the trader's timeframe. Switching re-keys
 * the chart — TradingChart's useLwcChart deps already recreate the
 * canvas and resubscribe `kline@{symbol}_{interval}`.
 */
import { lazy, useEffect, useRef, useState } from 'react';

import { KLINE_INTERVAL_SET, type KlineInterval } from '@/lib/market/channels';

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
  // Fill the panel box — the chart canvas measures its flex-1 slot
  // rather than rendering a fixed-height strip inside the tile (the
  // panel frame owns the outer height; h×ROW_H ≈ 830px for the default
  // 13-row chart panel).
  const boxRef = useRef<HTMLDivElement | null>(null);
  const [boxH, setBoxH] = useState<number | null>(null);
  useEffect(() => {
    const el = boxRef.current;
    if (!el || typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver(() => {
      if (el.clientHeight > 0) setBoxH(el.clientHeight);
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  const pick = (i: KlineInterval) => {
    setInterval(i);
    try {
      window.localStorage.setItem(STORAGE_KEY, i);
    } catch {
      /* non-fatal — session state still applies */
    }
  };
  return (
    <div className="flex h-full min-h-0 flex-col gap-1">
      <div className="flex gap-1" role="group" aria-label="Chart interval">
        {INTERVALS.map((i) => (
          <button
            key={i}
            type="button"
            aria-pressed={interval === i}
            onClick={() => pick(i)}
            className={`rounded px-2.5 py-1.5 font-mono text-xs ${
              interval === i
                ? 'bg-sky-700 text-white'
                : 'text-neutral-400 hover:bg-neutral-800 hover:text-neutral-200'
            }`}
          >
            {i}
          </button>
        ))}
      </div>
      <div ref={boxRef} className="min-h-0 flex-1">
        <TradingChart symbol={symbol} interval={interval} height={boxH ?? 320} />
      </div>
    </div>
  );
}
