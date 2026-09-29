/**
 * Support queue panel (Task 10.3.6) — ticket list from
 * GET /api/v1/admin/support/tickets plus queue counters. The list
 * envelope's `total` is the honest queue depth; per-status counters are
 * derived from the fetched page and labeled as such.
 */
import { useQuery } from '@tanstack/react-query';

import type { BoundAdminApi } from '@/lib/env';
import { fetchSupportTickets } from '@/lib/admin/api';
import { cardCls, tableCls, tdCls, thCls, ErrorBox, StatusBadge } from '@/lib/ui';
import { AccessDeniedCard } from './RequireAdmin';
import { isAccessDenied } from './adminRole';

export function SupportQueuePanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const query = useQuery({
    queryKey: ['admin', 'support-tickets', adminApi.env],
    queryFn: () => fetchSupportTickets(adminApi, { limit: 50 }),
    retry: false,
    refetchInterval: 30_000,
  });

  const tickets = query.data?.data ?? [];
  // Counters derived from the fetched page — labeled honestly.
  const openCount = tickets.filter((t) => t.status === 'OPEN' || t.status === 'IN_PROGRESS').length;
  const breachedCount = tickets.filter((t) => t.slaBreached).length;
  const byQueue = new Map<string, number>();
  for (const t of tickets) byQueue.set(t.queue, (byQueue.get(t.queue) ?? 0) + 1);

  return (
    <section className={cardCls} aria-label="Support queue">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">Support queue</h2>
      {query.error !== null && isAccessDenied(query.error) && (
        <AccessDeniedCard detail="The support queue requires Support Agent or auditor access." />
      )}
      {query.error !== null && !isAccessDenied(query.error) && <ErrorBox error={query.error} />}
      {query.error === null && (
        <>
          <div className="mb-2 flex gap-4 text-xs text-neutral-400">
            <span data-testid="queue-open">{openCount} open/in-progress</span>
            <span data-testid="queue-breached">{breachedCount} SLA breached</span>
            <span>{query.data?.total ?? tickets.length} total</span>
            {[...byQueue.entries()].map(([q, n]) => (
              <span key={q}>
                {q}: {n}
              </span>
            ))}
            <span className="text-neutral-600">(page-derived)</span>
          </div>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>#</th>
                <th className={thCls}>Subject</th>
                <th className={thCls}>Priority</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>SLA</th>
              </tr>
            </thead>
            <tbody>
              {tickets.map((t) => (
                <tr key={t.ticketId}>
                  <td className={tdCls}>{t.ticketId}</td>
                  <td className={tdCls}>{t.subject}</td>
                  <td className={tdCls}>{t.priority}</td>
                  <td className={tdCls}>
                    <StatusBadge value={t.status} />
                  </td>
                  <td className={tdCls}>
                    {t.slaBreached ? (
                      <span className="text-red-400">breached</span>
                    ) : (
                      (t.slaDueAt ?? '—')
                    )}
                  </td>
                </tr>
              ))}
              {tickets.length === 0 && !query.isLoading && (
                <tr>
                  <td className={tdCls} colSpan={5}>
                    Queue empty.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </>
      )}
    </section>
  );
}
