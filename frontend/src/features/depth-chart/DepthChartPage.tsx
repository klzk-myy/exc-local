/**
 * Depth chart page (`/depth/:symbol`) — the Task 10.3.12 chart plus a
 * raw-levels table so screen readers get the same cumulative view the
 * SVG shows visually (WCAG 2.1 AA: non-visual access to chart data).
 */
import { useParams } from 'react-router';

import { wsClient } from '@/app/runtime';
import { tableCls, tdCls, thCls } from '@/lib/ui';
import { formatPrice } from '@/lib/trading/fx';
import { cumulate } from '@/lib/trading/projections';
import { useDepthBook, useMarketFeed } from '@/lib/trading/marketStore';
import { useBookSnapshot, useInstrument } from '@/lib/trading/queries';

import { DepthChart } from './DepthChart';

function LevelTable({ symbol, client }: { symbol: string; client: typeof wsClient }) {
  useMarketFeed(symbol, client, { depth: true });
  const wsBook = useDepthBook(symbol);
  const rest = useBookSnapshot(symbol, 50);
  const book = wsBook ?? rest.data ?? undefined;
  const instrument = useInstrument(symbol);
  if (!book) return null;
  const bids = cumulate(book.bids).slice(0, 10);
  const asks = cumulate(book.asks).slice(0, 10);
  const n = Math.max(bids.length, asks.length);
  return (
    <div
      className="mt-4 relative overflow-x-auto rounded-lg border border-neutral-800 bg-neutral-900 p-4"
      tabIndex={0}
    >
      <h3 className="mb-2 text-sm font-semibold text-neutral-200">Levels (top 10 cumulative)</h3>
      <table className={tableCls}>
        <thead>
          <tr>
            <th className={thCls}>Bid px</th>
            <th className={thCls}>Bid cum qty</th>
            <th className={thCls}>Ask px</th>
            <th className={thCls}>Ask cum qty</th>
          </tr>
        </thead>
        <tbody>
          {Array.from({ length: n }, (_, i) => (
            <tr key={i}>
              <td className={tdCls}>{formatPrice(symbol, bids[i]?.price, instrument?.tickSize)}</td>
              <td className={tdCls}>{bids[i]?.cumQty.toDisplay(2) ?? ''}</td>
              <td className={tdCls}>{formatPrice(symbol, asks[i]?.price, instrument?.tickSize)}</td>
              <td className={tdCls}>{asks[i]?.cumQty.toDisplay(2) ?? ''}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export default function DepthChartPage() {
  const { symbol = 'EUR/USD' } = useParams<{ symbol: string }>();
  return (
    <div className="mx-auto max-w-4xl p-4">
      <h1 className="mb-4 text-2xl font-semibold">Market depth</h1>
      <DepthChart symbol={symbol} />
      <LevelTable symbol={symbol} client={wsClient} />
    </div>
  );
}
