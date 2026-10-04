/**
 * Client performance dashboard (Phase-10 Task 10.3.18) — P&L, per-pair
 * breakdown, win rate, fees.
 *
 * Truthfulness contract:
 *   - The aggregate endpoints (/account/pnl, /account/income,
 *     /account/snapshots, /reports/tca) are live (Phase-13/20); the page
 *     still probes them and renders an "unavailable" card on error —
 *     never fabricated aggregates.
 *   - All figures are computed client-side from the live order history +
 *     positions endpoints and carry a "derived" badge (FIFO-approximate
 *     matching — not a ledger view; the derivation rules are disclosed
 *     inline).
 */
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { lazy, Suspense, useRef } from 'react';

import { apiClient, wsClient } from '@/app/runtime';
import { RequireAuth } from '@/features/auth/guards';
import type { ApiClient } from '@/lib/api';
import { fetchPositions } from '@/lib/market/api';
import { useAuthed } from '@/lib/trading/queries';
import { PRIVATE_CHANNELS } from '@/lib/market/channels';
import { formatPnl } from '@/lib/market/format';
import { useChannel, useWsStatus, type WsClient } from '@/lib/ws';
import { cardCls, tableCls, tdCls, thCls, ErrorBox, StatusBadge } from '@/lib/ui';

import { fetchOrderHistory } from './api';
import { derivePerformance } from './derive';

const EquityCurveChart = lazy(() => import('./EquityCurveChart'));

/**
 * Live seam (Task 10.3.18 item 2): subscribe the auth-gated private
 * channels and invalidate the matching REST queries on each frame — the
 * snapshot rows remain the source of truth (frames trigger a refetch,
 * they are never merged blindly), and invalidations are coalesced to at
 * most one refetch per channel per 2s so a busy session cannot storm
 * the API.
 */
function PrivateFeed({ ws }: { ws: WsClient }) {
  const qc = useQueryClient();
  const lastInvalidate = useRef(0);
  const invalidate = (key: string) => {
    const now = Date.now();
    if (now - lastInvalidate.current < 2_000) return;
    lastInvalidate.current = now;
    void qc.invalidateQueries({ queryKey: ['performance', key] });
  };
  useChannel(ws, PRIVATE_CHANNELS.positions, () => invalidate('positions'));
  useChannel(ws, PRIVATE_CHANNELS.orders, () => invalidate('orders'));
  useChannel(ws, PRIVATE_CHANNELS.executions, () => invalidate('orders'));
  return null;
}

function DerivedBadge() {
  return (
    <span
      className="rounded bg-amber-500/20 px-2 py-0.5 text-xs font-medium text-amber-300"
      title="Computed client-side from order history + positions — not a ledger aggregate"
    >
      derived
    </span>
  );
}

function Stat({ label, value, badge }: { label: string; value: string; badge?: boolean }) {
  return (
    <div className={cardCls}>
      <p className="text-xs text-neutral-500">
        {label} {badge === true && <DerivedBadge />}
      </p>
      <p className="mt-1 text-xl font-semibold">{value}</p>
    </div>
  );
}

/** Probe a reporting endpoint; "available" means a 2xx parse, anything
 * else renders as unavailable. */
function useReportingProbe(api: ApiClient, path: string, label: string) {
  const authed = useAuthed();
  return useQuery({
    queryKey: ['performance', 'probe', path],
    queryFn: async () => {
      try {
        await api.get(path);
        return { label, available: true as const };
      } catch {
        return { label, available: false as const };
      }
    },
    retry: false,
    staleTime: 300_000,
    enabled: authed,
  });
}

export default function PerformancePage({
  api = apiClient,
  ws = wsClient,
}: {
  api?: ApiClient;
  ws?: WsClient;
}) {
  const wsStatus = useWsStatus(ws);
  const live = wsStatus.state === 'AUTHENTICATED';
  const authed = useAuthed();
  const positions = useQuery({
    queryKey: ['performance', 'positions'],
    queryFn: () => fetchPositions(api),
    retry: false,
    refetchInterval: 30_000,
    enabled: authed,
  });
  const orders = useQuery({
    queryKey: ['performance', 'orders'],
    queryFn: () => fetchOrderHistory(api, { limit: 200, pages: 3 }),
    retry: false,
    enabled: authed,
  });
  const pnlProbe = useReportingProbe(api, '/account/pnl', '/account/pnl');
  const incomeProbe = useReportingProbe(api, '/account/income', '/account/income');
  const snapshotsProbe = useReportingProbe(
    api,
    '/account/snapshots',
    '/account/snapshots (equity history)',
  );

  const summary =
    orders.data !== undefined ? derivePerformance(orders.data.data, positions.data ?? []) : null;

  const probes = [pnlProbe.data, incomeProbe.data, snapshotsProbe.data].filter(
    (p): p is { label: string; available: boolean } => p !== undefined,
  );
  const unavailable = probes.filter((p) => !p.available);

  if (!authed) {
    return <RequireAuth>{null}</RequireAuth>;
  }
  return (
    <div className="mx-auto max-w-6xl p-6">
      <PrivateFeed ws={ws} />
      <div className="mb-4 flex flex-wrap items-center justify-between gap-2">
        <h1 className="text-2xl font-semibold">Performance</h1>
        <span
          className={`rounded px-2 py-0.5 text-xs font-medium ${live ? 'bg-emerald-500/20 text-emerald-400' : 'bg-amber-500/20 text-amber-300'}`}
          data-testid="feed-status"
        >
          {live ? 'live updates' : `polling — socket ${wsStatus.state.toLowerCase()}`}
        </span>
      </div>
      <ErrorBox error={orders.error ?? positions.error} />

      {summary !== null && (
        <>
          <div className="mb-4 grid grid-cols-2 gap-4 md:grid-cols-4">
            <Stat label="Realized P&L" value={formatPnl(String(summary.realizedPnl))} badge />
            <Stat label="Unrealized P&L" value={formatPnl(String(summary.unrealizedPnl))} badge />
            <Stat
              label="Win rate"
              value={summary.winRate === null ? '—' : `${(summary.winRate * 100).toFixed(1)}%`}
              badge
            />
            <Stat label="Filled orders analyzed" value={String(summary.filledOrders)} badge />
          </div>

          <section className={`${cardCls} mb-4`} aria-label="Equity history">
            <h2 className="mb-2 text-sm font-medium text-neutral-400">P&L history</h2>
            <Suspense fallback={<p className="text-xs text-neutral-500">Loading chart…</p>}>
              <EquityCurveChart summary={summary} positions={positions.data ?? []} />
            </Suspense>
          </section>

          <section className={cardCls} aria-label="Per-pair breakdown">
            <h2 className="mb-2 text-sm font-medium text-neutral-400">
              Per-pair breakdown <DerivedBadge />
            </h2>
            <div className="relative overflow-x-auto" tabIndex={0}>
            <table className={tableCls}>
              <thead>
                <tr>
                  <th className={thCls}>Symbol</th>
                  <th className={thCls}>Bought</th>
                  <th className={thCls}>Sold</th>
                  <th className={thCls}>Notional</th>
                  <th className={thCls}>Realized</th>
                  <th className={thCls}>Unrealized</th>
                </tr>
              </thead>
              <tbody>
                {summary.perPair.map((p) => (
                  <tr key={p.symbol}>
                    <td className={tdCls}>{p.symbol}</td>
                    <td className={tdCls}>{p.filledBuyQty.toFixed(2)}</td>
                    <td className={tdCls}>{p.filledSellQty.toFixed(2)}</td>
                    <td className={tdCls}>{p.notional.toFixed(2)}</td>
                    <td
                      className={`${tdCls} ${p.realizedPnl >= 0 ? 'text-emerald-400' : 'text-red-400'}`}
                    >
                      {formatPnl(String(p.realizedPnl))}
                    </td>
                    <td
                      className={`${tdCls} ${p.unrealizedPnl >= 0 ? 'text-emerald-400' : 'text-red-400'}`}
                    >
                      {formatPnl(String(p.unrealizedPnl))}
                    </td>
                  </tr>
                ))}
                {summary.perPair.length === 0 && (
                  <tr>
                    <td className={tdCls} colSpan={6}>
                      No fills or positions yet.
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
            </div>
          </section>

          <section className={`${cardCls} mt-4`} aria-label="Open positions">
            <h2 className="mb-2 text-sm font-medium text-neutral-400">Open positions</h2>
            <div className="relative overflow-x-auto" tabIndex={0}>
            <table className={tableCls}>
              <thead>
                <tr>
                  <th className={thCls}>Symbol</th>
                  <th className={thCls}>Side</th>
                  <th className={thCls}>Qty</th>
                  <th className={thCls}>Entry</th>
                  <th className={thCls}>Unrealized</th>
                </tr>
              </thead>
              <tbody>
                {(positions.data ?? []).map((p) => (
                  <tr key={p.positionId}>
                    <td className={tdCls}>{p.symbol}</td>
                    <td className={tdCls}>
                      <StatusBadge value={p.side} />
                    </td>
                    <td className={tdCls}>{p.quantity}</td>
                    <td className={tdCls}>{p.entryPrice}</td>
                    <td className={tdCls}>{formatPnl(p.unrealizedPnl)}</td>
                  </tr>
                ))}
                {(positions.data ?? []).length === 0 && (
                  <tr>
                    <td className={tdCls} colSpan={5}>
                      No open positions.
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
            </div>
          </section>
        </>
      )}

      <section className={`${cardCls} mt-4`} aria-label="Reporting availability">
        <h2 className="mb-2 text-sm font-medium text-neutral-400">Reporting surfaces</h2>
        <ul className="space-y-1 text-xs text-neutral-500">
          {probes.map((p) => (
            <li key={p.label}>
              <code>{p.label}</code>:{' '}
              {p.available
                ? 'available'
                : 'unavailable (registered — handler lands in Phase-13/20)'}
            </li>
          ))}
          <li>
            Fees paid: shown once <code>/account/income</code> or fee-bearing fills land — order
            rows do not break out commission today.
          </li>
        </ul>
        {unavailable.length > 0 && (
          <p className="mt-2 text-xs text-amber-400" data-testid="derived-note">
            Figures above are client-side derivations (FIFO-approximate lot matching) until the
            reporting endpoints ship — they are approximations, not ledger truth.
          </p>
        )}
      </section>
    </div>
  );
}
