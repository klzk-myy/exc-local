/**
 * Market trades — the Binance-style recent-trades tape (Time & Sales).
 *
 * Consumes the canonical `trades@{symbol}` channel (Phase-06 Task 6.3.3)
 * via the shared useChannel seam and the wire-layer parseTradeEvent —
 * the raw multi-channel inspector on the Orders page (LiveTape) stays
 * the debug surface; this panel is the compact production tape.
 *
 * Rows are newest-first, capped at 50; BUY/SELL colors carry text labels
 * too (color is never the only aggressor signal).
 */
import { useRef, useState } from 'react';

import { wsClient } from '@/app/runtime';
import { formatPrice, metaFor, useInstruments } from '@/lib/input-helpers';
import { tradesChannel } from '@/lib/market/channels';
import { parseTradeEvent, type TradeEvent } from '@/lib/market/wire';
import { tableCls, tdCls, thCls } from '@/lib/ui';
import { useChannel, type WsClient } from '@/lib/ws';

const TAPE_LIMIT = 50;

export function MarketTrades({
  symbol,
  client = wsClient,
}: {
  symbol: string;
  client?: WsClient;
}) {
  const { instruments } = useInstruments();
  const meta = metaFor(instruments, symbol);
  const [rows, setRows] = useState<TradeEvent[]>([]);
  const seq = useRef(0);

  // Re-key on symbol — stale fills for the previous pair must never
  // render under the new label.
  const [rowsFor, setRowsFor] = useState(symbol);
  if (rowsFor !== symbol) {
    setRowsFor(symbol);
    setRows([]);
  }

  useChannel(client, tradesChannel(symbol), (frame) => {
    const t = parseTradeEvent(frame.data);
    if (!t) return;
    seq.current += 1;
    setRows((prev) => [{ ...t, tradeId: t.tradeId || seq.current }, ...prev].slice(0, TAPE_LIMIT));
  });

  return (
    <div className="h-full overflow-y-auto" role="log" aria-label={`${symbol} recent trades`}>
      <table className={tableCls}>
        <thead className="sticky top-0 bg-neutral-900">
          <tr>
            <th className={thCls}>Time</th>
            <th className={thCls}>Side</th>
            <th className={thCls}>Price</th>
            <th className={thCls}>Qty</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((t) => {
            const buy = t.side === 'BUY';
            return (
              <tr key={t.tradeId}>
                <td className={`${tdCls} font-mono text-neutral-400`}>
                  {t.tsMs > 0 ? new Date(t.tsMs).toLocaleTimeString() : '—'}
                </td>
                <td
                  className={`${tdCls} font-mono ${buy ? 'text-emerald-400' : 'text-red-400'}`}
                >
                  {t.side}
                </td>
                <td className={`${tdCls} font-mono`}>{formatPrice(meta, t.price)}</td>
                <td className={`${tdCls} font-mono`}>{t.quantity}</td>
              </tr>
            );
          })}
          {rows.length === 0 && (
            <tr>
              <td className={`${tdCls} text-neutral-500`} colSpan={4}>
                Waiting for prints…
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  );
}
