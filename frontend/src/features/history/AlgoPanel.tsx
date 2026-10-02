/**
 * Algo-order management (Task 10.3.27 item 2) — TWAP/VWAP/VP/grid
 * strategy list with pause/resume/cancel.
 *
 * All algo-management routes are live (Phase-16 Task 16.3.21 —
 * supersedes the earlier registered-stub note). The panel renders the
 * list from the endpoint; per-row controls call the pause/resume/cancel
 * paths and surface errors inline rather than pretending success.
 */
import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError } from '@/lib/api';
import { ErrorBox, btnDanger, btnGhost, tableCls, tdCls, thCls } from '@/lib/ui';
import { UnavailablePanel, isNotImplemented } from '@/lib/input-helpers';

import {
  cancelAlgoOrder,
  listAlgoOrders,
  pauseAlgoOrder,
  resumeAlgoOrder,
  type AlgoOrder,
} from './api';

export function AlgoPanel() {
  const queryClient = useQueryClient();
  const [rowErr, setRowErr] = useState<{ id: string; err: unknown } | null>(null);
  const q = useQuery({
    queryKey: ['history', 'algo-orders'],
    queryFn: () => listAlgoOrders(apiClient),
    retry: (n, e) => !(e instanceof ApiError && e.status === 501) && n < 2,
  });

  const act = useMutation({
    mutationFn: async ({ id, op }: { id: string; op: 'pause' | 'resume' | 'cancel' }) => {
      if (op === 'pause') return pauseAlgoOrder(apiClient, id);
      if (op === 'resume') return resumeAlgoOrder(apiClient, id);
      return cancelAlgoOrder(apiClient, id);
    },
    onError: (e, vars) => {
      setRowErr({ id: vars.id, err: e });
    },
    onSuccess: async () => {
      setRowErr(null);
      await queryClient.invalidateQueries({ queryKey: ['history', 'algo-orders'] });
    },
  });

  if (q.isPending) return <p className="text-sm text-neutral-500">Loading algo orders…</p>;
  if (q.isError) {
    if (isNotImplemented(q.error)) {
      return (
        <UnavailablePanel
          feature="Algo order management"
          owner="Phase-16 Task 16.3.21"
          note="TWAP/VWAP/VP/grid strategy listing, pause/resume and cancel arrive with the algo engine."
        />
      );
    }
    return <ErrorBox error={q.error} />;
  }

  const rows = q.data;
  return (
    <section aria-label="Algo orders" className="space-y-2">
      {rows.length === 0 ? (
        <p className="text-sm text-neutral-500">No algo orders running.</p>
      ) : (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>Symbol</th>
              <th className={thCls}>Strategy</th>
              <th className={thCls}>Side</th>
              <th className={thCls}>Qty</th>
              <th className={thCls}>Status</th>
              <th className={thCls} />
            </tr>
          </thead>
          <tbody>
            {rows.map((a: AlgoOrder) => (
              <tr key={a.id}>
                <td className={tdCls + ' font-mono'}>{a.symbol}</td>
                <td className={tdCls}>{a.algoType}</td>
                <td className={tdCls}>{a.side ?? '—'}</td>
                <td className={tdCls + ' font-mono'}>{a.quantity ?? '—'}</td>
                <td className={tdCls}>{a.status}</td>
                <td className={tdCls}>
                  <div className="flex items-center gap-1">
                    <button
                      type="button"
                      className={btnGhost}
                      disabled={act.isPending}
                      onClick={() => {
                        act.mutate({ id: a.id, op: a.status === 'PAUSED' ? 'resume' : 'pause' });
                      }}
                    >
                      {a.status === 'PAUSED' ? 'Resume' : 'Pause'}
                    </button>
                    <button
                      type="button"
                      className={btnDanger}
                      disabled={act.isPending}
                      onClick={() => {
                        act.mutate({ id: a.id, op: 'cancel' });
                      }}
                    >
                      Cancel
                    </button>
                    {rowErr?.id === a.id ? (
                      <span className="text-xs text-amber-400">
                        {isNotImplemented(rowErr.err)
                          ? 'not implemented (501)'
                          : rowErr.err instanceof Error
                            ? rowErr.err.message
                            : 'error'}
                      </span>
                    ) : null}
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
