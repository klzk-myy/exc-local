/**
 * OPO/OCO order lists (Task 10.3.27 item 3) — open lists + history tabs.
 * GET /api/v1/order-lists and /order-lists/history are registered stubs
 * (Phase-16 Task 16.3.20) — the panel renders parent/child rows only
 * when the backend serves them.
 */
import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError } from '@/lib/api';
import { ErrorBox, tableCls, tdCls, thCls } from '@/lib/ui';
import { UnavailablePanel, isNotImplemented } from '@/lib/input-helpers';

import { listOrderListHistory, listOrderLists, type OrderListSummary } from './api';

export function OrderListsPanel() {
  const [tab, setTab] = useState<'open' | 'history'>('open');
  const q = useQuery({
    queryKey: ['history', 'order-lists', tab],
    queryFn: () => (tab === 'open' ? listOrderLists(apiClient) : listOrderListHistory(apiClient)),
    retry: (n, e) => !(e instanceof ApiError && e.status === 501) && n < 2,
  });

  return (
    <section aria-label="Order lists" className="space-y-3">
      <div role="tablist" aria-label="Order list view" className="flex gap-1">
        {(['open', 'history'] as const).map((t) => (
          <button
            key={t}
            role="tab"
            aria-selected={tab === t}
            className={`rounded px-3 py-1 text-sm ${tab === t ? 'bg-neutral-800 text-neutral-100' : 'text-neutral-400 hover:text-neutral-200'}`}
            onClick={() => {
              setTab(t);
            }}
          >
            {t === 'open' ? 'Open lists' : 'List history'}
          </button>
        ))}
      </div>

      {q.isPending ? (
        <p className="text-sm text-neutral-500">Loading order lists…</p>
      ) : q.isError ? (
        isNotImplemented(q.error) ? (
          <UnavailablePanel
            feature="OPO / OCO order lists"
            owner="Phase-16 Task 16.3.20"
            note="Parent/child list state arrives with the order-list engine."
          />
        ) : (
          <ErrorBox error={q.error} />
        )
      ) : q.data.length === 0 ? (
        <p className="text-sm text-neutral-500">No {tab} order lists.</p>
      ) : (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>List</th>
              <th className={thCls}>Type</th>
              <th className={thCls}>Symbol</th>
              <th className={thCls}>Legs</th>
              <th className={thCls}>Status</th>
            </tr>
          </thead>
          <tbody>
            {q.data.map((l: OrderListSummary) => (
              <tr key={l.id}>
                <td className={tdCls + ' font-mono'}>{l.id}</td>
                <td className={tdCls}>{l.type}</td>
                <td className={tdCls + ' font-mono'}>{l.symbol ?? '—'}</td>
                <td className={tdCls}>{l.legs ?? '—'}</td>
                <td className={tdCls}>{l.status}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
