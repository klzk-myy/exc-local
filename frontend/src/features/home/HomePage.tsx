/**
 * Dashboard — landing surface. Live tiles for the WS state machine
 * (Task 10.3.19) and the exchange clock, plus an authenticated account
 * snapshot (balances / open positions / open orders) and quick links
 * into the trading surfaces.
 *
 * State surfaces:
 *   - Zustand for local UI state (density preference)
 *   - TanStack Query for server state (time, balances, positions, orders)
 *   - useWsStatus for the WS state machine
 */
import { useQuery } from '@tanstack/react-query';
import { Link } from 'react-router';
import { create } from 'zustand';

import { apiClient, wsClient } from '@/app/runtime';
import { useSessionStore } from '@/lib/auth/session';
import { useBalances, useOpenOrders, usePositions } from '@/lib/trading/queries';
import { tableCls, tdCls, thCls, cardCls, ErrorBox } from '@/lib/ui';
import { useWsStatus } from '@/lib/ws';

const useDashboardPrefs = create<{ compact: boolean; toggleCompact: () => void }>((set) => ({
  compact: false,
  toggleCompact: () => set((s) => ({ compact: !s.compact })),
}));

// Public exchange clock (§21.11 surface).
function useServerTime() {
  return useQuery({
    queryKey: ['system', 'time'],
    queryFn: () => apiClient.get<{ server_time_ms: number }>('/time'),
    refetchInterval: 30_000,
    retry: false,
  });
}

const QUICK_LINKS = [
  { label: 'Workspace', to: '/workspace' },
  { label: 'Chart', to: '/chart/EUR%2FUSD' },
  { label: 'Orders', to: '/orders' },
  { label: 'Calculator', to: '/calculator' },
  { label: 'Portfolio', to: '/portfolio' },
];

function AccountSnapshot() {
  const balances = useBalances();
  const positions = usePositions();
  const { orders: openOrders, query: ordersQuery } = useOpenOrders();
  const queryError = balances.error ?? positions.error ?? ordersQuery.error;

  return (
    <section className={cardCls} aria-label="Account snapshot">
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-sm font-medium text-neutral-400">Account snapshot</h2>
        <Link to="/portfolio" className="text-xs text-sky-400 hover:underline">
          Full portfolio →
        </Link>
      </div>
      <ErrorBox error={queryError} />
      <div className="grid gap-4 md:grid-cols-3">
        <div>
          <h3 className="mb-1 text-xs font-medium text-neutral-500">Balances</h3>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>CCY</th>
                <th className={thCls}>Available</th>
                <th className={thCls}>Locked</th>
                <th className={thCls}>Total</th>
              </tr>
            </thead>
            <tbody>
              {(balances.data ?? []).map((b) => (
                <tr key={b.currency}>
                  <td className={tdCls}>{b.currency}</td>
                  <td className={tdCls}>{b.available.toDisplay(2)}</td>
                  <td className={tdCls}>{b.locked.toDisplay(2)}</td>
                  <td className={tdCls}>{b.total.toDisplay(2)}</td>
                </tr>
              ))}
              {balances.data?.length === 0 && (
                <tr>
                  <td className={tdCls} colSpan={4}>
                    No balances — fund the account to trade.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
        <div>
          <h3 className="mb-1 text-xs font-medium text-neutral-500">Open positions</h3>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Symbol</th>
                <th className={thCls}>Side</th>
                <th className={thCls}>Qty</th>
                <th className={thCls}>uPnL</th>
              </tr>
            </thead>
            <tbody>
              {(positions.data ?? []).map((p) => (
                <tr key={p.id}>
                  <td className={tdCls}>
                    <Link
                      to={`/advanced/${encodeURIComponent(p.symbol)}`}
                      className="text-sky-400 hover:underline"
                    >
                      {p.symbol}
                    </Link>
                  </td>
                  <td className={tdCls}>{p.side}</td>
                  <td className={tdCls}>{p.quantity.toDisplay()}</td>
                  <td className={tdCls}>{p.unrealizedPnl.toDisplay(2)}</td>
                </tr>
              ))}
              {positions.data?.length === 0 && (
                <tr>
                  <td className={tdCls} colSpan={4}>
                    No open positions.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
        <div>
          <h3 className="mb-1 text-xs font-medium text-neutral-500">Open orders</h3>
          <p className="text-2xl font-semibold">{openOrders.length}</p>
          {openOrders.length > 0 && (
            <ul className="mt-1 space-y-0.5 text-xs text-neutral-400">
              {openOrders.slice(0, 5).map((o) => (
                <li key={o.id}>
                  {o.symbol} {o.side} {o.type}
                </li>
              ))}
            </ul>
          )}
          <Link to="/orders" className="mt-2 inline-block text-xs text-sky-400 hover:underline">
            Order history →
          </Link>
        </div>
      </div>
    </section>
  );
}

export default function HomePage() {
  const status = useWsStatus(wsClient);
  const compact = useDashboardPrefs((s) => s.compact);
  const toggleCompact = useDashboardPrefs((s) => s.toggleCompact);
  const serverTime = useServerTime();
  const signedIn = useSessionStore((s) => s.accessToken !== null);

  const staleChannels = Object.entries(status.health).filter(([, h]) => h.stale);

  return (
    <div className={`mx-auto max-w-6xl p-6 ${compact ? 'text-sm' : ''}`}>
      <div className="mb-6 flex items-center justify-between">
        <h1 className="text-2xl font-semibold">Dashboard</h1>
        <button
          type="button"
          onClick={toggleCompact}
          className="rounded border border-neutral-700 px-3 py-1 text-sm text-neutral-300 hover:bg-neutral-800"
        >
          {compact ? 'Comfortable' : 'Compact'} density
        </button>
      </div>

      <div className="grid gap-4 md:grid-cols-3">
        <section className={cardCls} aria-label="Connection">
          <h2 className="mb-2 text-sm font-medium text-neutral-400">Connection</h2>
          <p className="text-lg font-semibold" data-testid="ws-state">
            {status.state}
          </p>
          <p className="mt-1 text-xs text-neutral-500">
            Order entry {status.orderEntryEnabled ? 'enabled' : 'locked'}
          </p>
        </section>

        <section className={cardCls} aria-label="Subscriptions">
          <h2 className="mb-2 text-sm font-medium text-neutral-400">Subscriptions</h2>
          <p className="text-lg font-semibold">{status.subscriptions.length}</p>
          {staleChannels.length > 0 && (
            <p className="mt-1 text-xs text-amber-400">{staleChannels.length} channel(s) stale</p>
          )}
        </section>

        <section className={cardCls} aria-label="Exchange time">
          <h2 className="mb-2 text-sm font-medium text-neutral-400">Exchange time (UTC)</h2>
          <p className="text-lg font-semibold" data-testid="server-time">
            {serverTime.data && Number.isFinite(serverTime.data.server_time_ms)
              ? new Date(serverTime.data.server_time_ms).toISOString()
              : '—'}
          </p>
        </section>
      </div>

      <div className="mt-4">
        {signedIn ? (
          <AccountSnapshot />
        ) : (
          <section className={cardCls} aria-label="Sign in">
            <h2 className="mb-2 text-sm font-medium text-neutral-400">Account</h2>
            <p className="text-sm text-neutral-500">
              <Link to="/login" className="text-sky-400 hover:underline">
                Sign in
              </Link>{' '}
              to see balances, positions, and open orders here.
            </p>
          </section>
        )}
      </div>

      <nav aria-label="Quick links" className="mt-4 flex flex-wrap gap-2">
        {QUICK_LINKS.map((l) => (
          <Link
            key={l.to}
            to={l.to}
            className="rounded border border-neutral-700 px-3 py-1.5 text-sm text-neutral-300 hover:bg-neutral-800 hover:text-neutral-100"
          >
            {l.label}
          </Link>
        ))}
      </nav>
    </div>
  );
}
