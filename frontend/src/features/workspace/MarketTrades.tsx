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
import { useEffect, useRef, useState } from 'react';

import { wsClient } from '@/app/runtime';
import { formatPrice, metaFor, useInstruments } from '@/lib/input-helpers';
import { tradesChannel } from '@/lib/market/channels';
import { parseTradeEvent, type TradeEvent } from '@/lib/market/wire';
import { tableCls, tdCls, thCls } from '@/lib/ui';
import { useChannel, type WsClient } from '@/lib/ws';

const TAPE_LIMIT = 50;
/** Coalesce bursts — a hot market can print many frames per second; one
 * re-render per 100ms window keeps the tape live without churn. */
const FLUSH_MS = 100;

export function MarketTrades({ symbol, client = wsClient }: { symbol: string; client?: WsClient }) {
  const { instruments } = useInstruments();
  const meta = metaFor(instruments, symbol);
  const [rows, setRows] = useState<TradeEvent[]>([]);
  const seq = useRef(0);
  const pending = useRef<TradeEvent[]>([]);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Re-key on symbol — stale fills for the previous pair must never
  // render under the new label.
  const [rowsFor, setRowsFor] = useState(symbol);
  if (rowsFor !== symbol) {
    setRowsFor(symbol);
    setRows([]);
    pending.current = [];
  }
  useEffect(
    () => () => {
      if (timer.current !== null) clearTimeout(timer.current);
    },
    [],
  );

  useChannel(client, tradesChannel(symbol), (frame) => {
    const t = parseTradeEvent(frame.data);
    if (!t) return;
    seq.current += 1;
    pending.current.unshift({ ...t, tradeId: t.tradeId || seq.current });
    timer.current ??= setTimeout(() => {
      timer.current = null;
      const batch = pending.current;
      pending.current = [];
      if (batch.length === 0) return;
      setRows((prev) => [...batch, ...prev].slice(0, TAPE_LIMIT));
    }, FLUSH_MS);
  });

  return (
    <div
      className="h-full overflow-y-auto"
      role="log"
      tabIndex={0}
      aria-label={`${symbol} recent trades`}
    >
      <table className={tableCls}>
        <thead className="sticky top-0 bg-neutral-900">
          <tr>
            <th className={thCls}>Price</th>
            <th className={thCls}>Qty</th>
            <th className={thCls}>Side</th>
            <th className={thCls}>Time</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((t) => {
            const buy = t.side === 'BUY';
            return (
              <tr key={t.tradeId}>
                <td className={`${tdCls} font-mono ${buy ? 'text-emerald-400' : 'text-red-400'}`}>
                  {formatPrice(meta, t.price)}
                </td>
                <td className={`${tdCls} font-mono`}>{t.quantity}</td>
                <td className={`${tdCls} font-mono ${buy ? 'text-emerald-400' : 'text-red-400'}`}>
                  {t.side}
                </td>
                <td className={`${tdCls} font-mono text-neutral-400`}>
                  {t.tsMs > 0 ? new Date(t.tsMs).toLocaleTimeString() : '—'}
                </td>
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
