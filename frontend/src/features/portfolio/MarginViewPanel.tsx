/**
 * Account positions view (Task 10.5.3.27 gate-coverage wiring,
 * Phase-19 Task 19.3.15) — GET /account/positions returns the
 * netting/hedging-mode-aware enriched view (position_mode, margin
 * level, ADL quintile decoration). Distinct from the real-time
 * private:positions feed above: this is the server-decorated margin
 * read, rendered verbatim so every decorated field stays visible.
 */
import { useQuery } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ErrorBox, JsonRows, cardCls } from '@/lib/ui';

export function MarginViewPanel() {
  const q = useQuery({
    queryKey: ['account', 'positions-margin-view'],
    queryFn: () => apiClient.get<unknown>('/account/positions'),
    retry: false,
  });
  const rows: unknown[] = (() => {
    const d: unknown = q.data;
    if (typeof d === 'object' && d !== null) {
      const p = (d as Record<string, unknown>)['positions'];
      if (Array.isArray(p)) return p as unknown[];
    }
    return Array.isArray(d) ? (d as unknown[]) : [];
  })();

  return (
    <section className={`${cardCls} mt-4`} aria-label="Margin positions view">
      <div className="flex items-center justify-between">
        <h2 className="text-sm font-semibold">Margin view (decorated)</h2>
        <button
          type="button"
          className="text-xs text-sky-300 hover:underline"
          onClick={() => void q.refetch()}
        >
          Refresh
        </button>
      </div>
      <p className="mb-2 mt-1 text-xs text-neutral-500">
        Netting/hedging-aware positions with margin level and ADL quintile decoration (Phase-19 Task
        19.3.15).
      </p>
      {q.isError && <ErrorBox error={q.error} />}
      {q.isPending ? (
        <p className="text-sm text-neutral-400">Loading margin view…</p>
      ) : (
        <JsonRows rows={rows} empty="No positions in the decorated view." />
      )}
    </section>
  );
}
