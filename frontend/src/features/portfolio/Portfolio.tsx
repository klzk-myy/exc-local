/**
 * Portfolio — positions + balances panels (Task 10.3.5).
 *
 *   - Positions: symbol / side / qty / entry / mark / unrealized P&L /
 *     margin — real-time via private:positions
 *   - Balances: currency / free (available) / used (locked) / total —
 *     real-time via private:balances
 *   - STALE MARKS badge ties mark-price freshness to the ws channel
 *     health surface (Task 10.3.19 — >3s silence flags stale)
 *   - Close = opposing reduce-only MARKET order through the idempotent
 *     order pipeline (there is no per-position REST close endpoint;
 *     /positions/close-all is bulk-only, so reduce-only market is the
 *     canonical per-position close)
 */
import { useState } from 'react';

import { apiClient, wsClient } from '@/app/runtime';
import { ApiError, type ApiClient } from '@/lib/api';
import { useSessionStore } from '@/lib/auth/session';
import { submitOrder } from '@/lib/market/api';
import {
  formatCurrency,
  formatDecimal,
  formatPrice,
  formatPnl,
  formatQty,
} from '@/lib/market/format';
import { useInstruments } from '@/lib/market/hooks';
import { usePendingOrders } from '@/lib/market/pending';
import type { PositionRow } from '@/lib/market/wire';
import type { WsClient } from '@/lib/ws';
import { useWsStatus } from '@/lib/ws';

import { usePortfolio } from './usePortfolio';

const thCls =
  'px-2 py-1.5 text-left text-[10px] font-medium uppercase tracking-wide text-neutral-500';
const tdCls = 'px-2 py-1.5 font-mono text-xs text-neutral-200';

function CloseButton(props: { position: PositionRow; api: ApiClient; disabled: boolean }) {
  const { position, api, disabled } = props;
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const epoch = useSessionStore((s) => s.authEpoch);

  async function close() {
    setBusy(true);
    setError(null);
    const cid = `web-close-${crypto.randomUUID()}`;
    usePendingOrders.getState().add({
      clientOrderId: cid,
      symbol: position.symbol,
      side: position.side === 'LONG' ? 'SELL' : 'BUY',
      type: 'MARKET',
      quantity: position.quantity,
      epoch,
    });
    try {
      const ack = await submitOrder(api, {
        symbol: position.symbol,
        side: position.side === 'LONG' ? 'SELL' : 'BUY',
        type: 'MARKET',
        time_in_force: 'IOC',
        client_order_id: cid,
        quantity: position.quantity,
        reduce_only: true,
      });
      usePendingOrders.getState().confirm(cid, `Close submitted (id ${ack.orderId})`);
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : 'close order failed';
      const code = e instanceof ApiError ? e.code : undefined;
      usePendingOrders.getState().reject(cid, {
        message: `Close failed: ${msg}`,
        code,
        requestId: e instanceof ApiError ? e.requestId : undefined,
      });
      setError(msg);
    } finally {
      setBusy(false);
    }
  }

  return (
    <span className="inline-flex items-center gap-1">
      <button
        type="button"
        disabled={disabled || busy}
        onClick={() => void close()}
        aria-label={`Close ${position.symbol} ${position.side}`}
        className="rounded border border-red-900 px-2 py-0.5 text-[10px] font-medium text-red-400 hover:bg-red-950/50 disabled:cursor-not-allowed disabled:opacity-40"
      >
        {busy ? 'Closing…' : 'Close'}
      </button>
      {error !== null && (
        <span role="alert" className="text-[10px] text-red-400">
          {error}
        </span>
      )}
    </span>
  );
}

export interface PortfolioProps {
  api?: ApiClient;
  ws?: WsClient;
}

export function Portfolio(props: PortfolioProps) {
  const api = props.api ?? apiClient;
  const ws = props.ws ?? wsClient;
  const { positions, balances, loading, error, marksStale, reload } = usePortfolio({ api, ws });
  const { bySymbol } = useInstruments(api);
  const status = useWsStatus(ws);

  return (
    <div className="grid gap-4 lg:grid-cols-3">
      <section
        aria-label="Open positions"
        className="min-w-0 rounded-lg border border-neutral-800 bg-neutral-900 lg:col-span-2"
      >
        <header className="flex items-center justify-between border-b border-neutral-800 px-3 py-2">
          <h2 className="text-sm font-semibold">Positions</h2>
          {marksStale && (
            <span
              role="status"
              className="rounded bg-amber-500/20 px-1.5 py-0.5 text-[10px] font-semibold text-amber-400"
            >
              STALE MARKS
            </span>
          )}
        </header>
        {loading ? (
          <p className="p-4 text-center text-sm text-neutral-500" role="status">
            Loading positions…
          </p>
        ) : error !== null ? (
          <div className="p-4 text-center">
            <p role="alert" className="text-sm text-red-400">
              {error}
            </p>
            <button
              type="button"
              onClick={reload}
              className="mt-2 rounded border border-neutral-700 px-3 py-1 text-xs text-neutral-300 hover:bg-neutral-800"
            >
              Retry
            </button>
          </div>
        ) : positions.length === 0 ? (
          <p className="p-4 text-center text-sm text-neutral-500">No open positions</p>
        ) : (
          <div className="relative overflow-x-auto" tabIndex={0}>
            <table className="w-full">
              <thead>
                <tr className="border-b border-neutral-800">
                  <th className={thCls}>Symbol</th>
                  <th className={thCls}>Side</th>
                  <th className={`${thCls} text-right`}>Qty</th>
                  <th className={`${thCls} text-right`}>Entry</th>
                  <th className={`${thCls} text-right`}>Mark</th>
                  <th className={`${thCls} text-right`}>uPnL</th>
                  <th className={`${thCls} text-right`}>Margin</th>
                  <th className={thCls}>
                    <span className="sr-only">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {positions.map((p) => {
                  const inst = bySymbol.get(p.symbol);
                  const pnl = p.unrealizedPnl;
                  const pnlCls = pnl.startsWith('-')
                    ? 'text-red-400'
                    : pnl === '0'
                      ? 'text-neutral-400'
                      : 'text-emerald-400';
                  return (
                    <tr
                      key={`${p.positionId}-${p.symbol}-${p.side}`}
                      className="border-b border-neutral-800/60"
                    >
                      <td className={`${tdCls} text-neutral-100`}>{p.symbol}</td>
                      <td className={tdCls}>
                        <span
                          className={`rounded px-1.5 py-0.5 text-[10px] font-semibold ${
                            p.side === 'LONG'
                              ? 'bg-emerald-500/15 text-emerald-400'
                              : 'bg-red-500/15 text-red-400'
                          }`}
                        >
                          {p.side}
                        </span>
                      </td>
                      <td className={`${tdCls} text-right`}>
                        {inst ? formatQty(p.quantity, inst) : p.quantity}
                      </td>
                      <td className={`${tdCls} text-right`}>
                        {inst ? formatPrice(p.entryPrice, inst) : p.entryPrice}
                      </td>
                      <td className={`${tdCls} text-right`}>
                        {p.markPrice !== undefined
                          ? inst
                            ? formatPrice(p.markPrice, inst)
                            : p.markPrice
                          : '—'}
                      </td>
                      <td className={`${tdCls} text-right ${pnlCls}`}>{formatPnl(pnl)}</td>
                      <td className={`${tdCls} text-right`}>{formatCurrency(p.marginUsed)}</td>
                      <td className={`${tdCls} text-right`}>
                        <CloseButton position={p} api={api} disabled={!status.orderEntryEnabled} />
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section
        aria-label="Account balances"
        className="min-w-0 rounded-lg border border-neutral-800 bg-neutral-900"
      >
        <header className="border-b border-neutral-800 px-3 py-2">
          <h2 className="text-sm font-semibold">Balances</h2>
        </header>
        {loading ? (
          <p className="p-4 text-center text-sm text-neutral-500" role="status">
            Loading balances…
          </p>
        ) : balances.length === 0 && error === null ? (
          <p className="p-4 text-center text-sm text-neutral-500">No balances</p>
        ) : (
          <div className="relative overflow-x-auto" tabIndex={0}>
            <table className="w-full">
              <thead>
                <tr className="border-b border-neutral-800">
                  <th className={thCls}>Currency</th>
                  <th className={`${thCls} text-right`}>Free</th>
                  <th className={`${thCls} text-right`}>Used</th>
                  <th className={`${thCls} text-right`}>Total</th>
                </tr>
              </thead>
              <tbody>
                {balances.map((b) => (
                  <tr key={b.currency} className="border-b border-neutral-800/60">
                    <td className={`${tdCls} text-neutral-100`}>{b.currency}</td>
                    <td className={`${tdCls} text-right`}>{formatCurrency(b.available)}</td>
                    <td className={`${tdCls} text-right text-neutral-400`}>
                      {formatDecimal(b.locked, 2)}
                    </td>
                    <td className={`${tdCls} text-right`}>{formatCurrency(b.total)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  );
}
