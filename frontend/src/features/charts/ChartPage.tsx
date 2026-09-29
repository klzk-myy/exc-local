/**
 * ChartPage — `/chart/:symbol` (Task 10.3.4). The chart itself is a
 * nested React.lazy boundary so `lightweight-charts` gets its own chunk
 * rather than inflating the page chunk (budget gate, Task 10.3.1).
 * Interval selector covers the canonical 13 timeframes (§24 #264).
 */
import { lazy, Suspense, useState } from 'react';
import { useParams } from 'react-router';

import { KLINE_INTERVALS, type KlineInterval } from '@/lib/market/channels';

const TradingChart = lazy(() => import('./TradingChart'));

export default function ChartPage() {
  const { symbol = 'EUR/USD' } = useParams();
  const [interval, setInterval] = useState<KlineInterval>('1h');
  return (
    <div className="mx-auto max-w-5xl p-4">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <h1 className="text-sm font-semibold text-neutral-200">
          {symbol} <span className="text-neutral-500">candlestick</span>
        </h1>
        <div className="flex flex-wrap gap-1" role="group" aria-label="Timeframe">
          {KLINE_INTERVALS.map((tf) => (
            <button
              key={tf}
              type="button"
              aria-pressed={interval === tf}
              onClick={() => setInterval(tf)}
              className={`rounded px-2 py-0.5 text-xs font-medium ${
                interval === tf
                  ? 'bg-neutral-700 text-white'
                  : 'text-neutral-400 hover:bg-neutral-800 hover:text-neutral-200'
              }`}
            >
              {tf}
            </button>
          ))}
        </div>
      </div>
      <Suspense
        fallback={
          <div
            className="flex h-80 items-center justify-center rounded-lg border border-neutral-800 text-sm text-neutral-500"
            role="status"
          >
            Loading chart…
          </div>
        }
      >
        <TradingChart symbol={symbol} interval={interval} height={360} />
      </Suspense>
    </div>
  );
}
