/**
 * Dashboard shell — the reference feature for the auto-discovery
 * manifest. Demonstrates the three sanctioned state surfaces:
 *   - Zustand for local UI state (compact-mode toggle)
 *   - TanStack Query for server state (GET /api/v1/time)
 *   - useWsStatus/useChannel for the WS state machine (Task 10.3.19)
 *
 * Wave-2 features (order book, charts, order entry) follow this shape —
 * see frontend/README.md.
 */
import { useQuery } from '@tanstack/react-query';
import { create } from 'zustand';

import { apiClient, wsClient } from '@/app/runtime';
import { useWsStatus } from '@/lib/ws';

// Local UI state — Zustand (spec §21.2).
const useDashboardPrefs = create<{ compact: boolean; toggleCompact: () => void }>((set) => ({
  compact: false,
  toggleCompact: () => set((s) => ({ compact: !s.compact })),
}));

// Server state — TanStack Query (public exchange clock, §21.11 surface).
function useServerTime() {
  return useQuery({
    queryKey: ['system', 'time'],
    queryFn: () => apiClient.get<{ ts_ms: number }>('/time'),
    refetchInterval: 30_000,
    retry: false,
  });
}

export default function HomePage() {
  const status = useWsStatus(wsClient);
  const compact = useDashboardPrefs((s) => s.compact);
  const toggleCompact = useDashboardPrefs((s) => s.toggleCompact);
  const serverTime = useServerTime();

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
        <section className="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
          <h2 className="mb-2 text-sm font-medium text-neutral-400">Connection</h2>
          <p className="text-lg font-semibold" data-testid="ws-state">
            {status.state}
          </p>
          <p className="mt-1 text-xs text-neutral-500">
            Order entry {status.orderEntryEnabled ? 'enabled' : 'locked'}
          </p>
        </section>

        <section className="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
          <h2 className="mb-2 text-sm font-medium text-neutral-400">Subscriptions</h2>
          <p className="text-lg font-semibold">{status.subscriptions.length}</p>
          {staleChannels.length > 0 && (
            <p className="mt-1 text-xs text-amber-400">{staleChannels.length} channel(s) stale</p>
          )}
        </section>

        <section className="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
          <h2 className="mb-2 text-sm font-medium text-neutral-400">Exchange time (UTC)</h2>
          <p className="text-lg font-semibold" data-testid="server-time">
            {serverTime.data ? new Date(serverTime.data.ts_ms).toISOString() : '—'}
          </p>
        </section>
      </div>

      <p className="mt-8 text-sm text-neutral-500">
        Trading cockpit surfaces arrive in Wave-2. This shell proves routing auto-discovery, the WS
        state machine, server-state wiring, and the security/budget gates.
      </p>
    </div>
  );
}
