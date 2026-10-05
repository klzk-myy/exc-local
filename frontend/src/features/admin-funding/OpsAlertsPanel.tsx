/**
 * Ops-alerts rail (Phase-10.5 Task 10.5.3.7 §5) — the durable funding
 * alert trail (nostro insufficiency, dispatch failures, deposit review
 * breaches). This is the queue-discovery feed for the id-driven
 * deposit/withdrawal action surfaces above.
 */
import { useQuery } from '@tanstack/react-query';

import type { BoundAdminApi } from '@/lib/env';
import { btnGhost, cardCls, ErrorBox, StatusBadge, tableCls, tdCls, thCls } from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import { fetchOpsAlerts } from './api';

export function OpsAlertsPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const list = useQuery({
    queryKey: ['admin-funding-ops-alerts'],
    queryFn: () => fetchOpsAlerts(adminApi),
  });

  if (list.error !== null && isAccessDenied(list.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Funding alerts">
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-sm font-semibold">Funding ops alerts</h2>
        <button
          type="button"
          className={btnGhost}
          onClick={() => void list.refetch()}
          disabled={list.isFetching}
        >
          Refresh
        </button>
      </div>
      {list.error !== null ? <ErrorBox error={list.error} /> : null}
      {list.data?.length === 0 ? (
        <p className="text-sm text-neutral-500">No funding alerts.</p>
      ) : null}
      {list.data !== undefined && list.data.length > 0 ? (
        <div className="max-h-56 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Code</th>
                <th className={thCls}>Severity</th>
                <th className={thCls}>Txn</th>
                <th className={thCls}>Account</th>
                <th className={thCls}>Summary</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>At</th>
              </tr>
            </thead>
            <tbody>
              {list.data.map((a) => (
                <tr key={a.id}>
                  <td className={tdCls}>{a.id}</td>
                  <td className={tdCls}>{a.code}</td>
                  <td className={tdCls}>
                    <StatusBadge value={a.severity || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>{a.fundingTransactionId ?? '—'}</td>
                  <td className={tdCls}>{a.accountId ?? '—'}</td>
                  <td className={tdCls}>{a.summary}</td>
                  <td className={tdCls}>
                    <StatusBadge value={a.status || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>{a.createdAt}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
    </section>
  );
}
