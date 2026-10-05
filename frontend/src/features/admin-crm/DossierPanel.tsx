/**
 * Customer dossier panel (Phase-10.5 Task 10.5.3.4 §1) — the
 * support-view aggregation (status/KYC/balances/orders/tickets/docs)
 * plus active compliance holds and an audit excerpt for the account.
 * Reads: GET /admin/support/accounts/{id} (audit-logged server-side),
 * GET /admin/compliance/holds, GET /admin/audit-log?target_*.
 */
import { useQuery } from '@tanstack/react-query';

import { fetchAuditLog, fetchSupportAccount } from '@/lib/admin/api';
import type { BoundAdminApi } from '@/lib/env';
import { cardCls, ErrorBox, StatusBadge } from '@/lib/ui';

import { AccessDeniedCard } from '../admin/RequireAdmin';
import { isAccessDenied } from '../admin/adminRole';
import { listHolds } from '../admin/consoles-api';

export function DossierPanel({
  adminApi,
  accountId,
}: {
  adminApi: BoundAdminApi;
  accountId: number;
}) {
  const dossier = useQuery({
    queryKey: ['admin-crm', 'dossier', adminApi.env, accountId],
    queryFn: () => fetchSupportAccount(adminApi, accountId),
    retry: false,
  });
  const holds = useQuery({
    queryKey: ['admin-crm', 'holds', adminApi.env, accountId],
    queryFn: () => listHolds(adminApi, 'OPEN'),
    retry: false,
    select: (all) => all.filter((h) => h.accountId === accountId),
  });
  const audit = useQuery({
    queryKey: ['admin-crm', 'audit', adminApi.env, accountId],
    queryFn: () =>
      fetchAuditLog(adminApi, { targetType: 'account', targetId: accountId, limit: 10 }),
    retry: false,
  });

  if (isAccessDenied(dossier.error)) {
    return <AccessDeniedCard detail="Customer 360 requires the Support Agent role." />;
  }

  const v = dossier.data;
  return (
    <section className={cardCls} aria-label="Customer dossier">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">Dossier — account #{accountId}</h2>
      {dossier.isError && <ErrorBox error={dossier.error} />}
      {v !== undefined && (
        <div data-testid="support-view">
          <div className="flex flex-wrap items-center gap-3 text-sm">
            <StatusBadge value={v.status.toUpperCase()} />
            <span className="text-neutral-400">KYC {v.kycTier}</span>
            <span className="text-neutral-400">{v.accountType}</span>
            <span className="text-neutral-500">opened {v.createdAt}</span>
          </div>
          {v.balances.length > 0 && (
            <div className="mt-2">
              <h3 className="text-xs font-medium text-neutral-400">Balances</h3>
              <ul className="text-xs text-neutral-300">
                {v.balances.map((b) => (
                  <li key={b.currency}>
                    {b.currency}: {b.available} available · {b.locked} locked
                  </li>
                ))}
              </ul>
            </div>
          )}
          {v.recentOrders.length > 0 && (
            <div className="mt-2">
              <h3 className="text-xs font-medium text-neutral-400">Recent orders</h3>
              <ul className="text-xs text-neutral-300">
                {v.recentOrders.map((o) => (
                  <li key={o.id}>
                    #{o.id} {o.side} {o.quantity} {o.symbol ?? ''} — {o.status}
                  </li>
                ))}
              </ul>
            </div>
          )}
          {v.kycDocuments.length > 0 && (
            <div className="mt-2">
              <h3 className="text-xs font-medium text-neutral-400">KYC documents</h3>
              <ul className="text-xs text-neutral-300">
                {v.kycDocuments.map((d) => (
                  <li key={d.id}>
                    #{d.id} {d.type} — {d.status}
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>
      )}

      <div className="mt-3">
        <h3 className="text-xs font-medium text-neutral-400">Active holds</h3>
        {holds.isError && <ErrorBox error={holds.error} />}
        {holds.isSuccess &&
          (holds.data.length === 0 ? (
            <p className="text-xs text-neutral-500">No open compliance holds.</p>
          ) : (
            <ul className="text-xs text-amber-300">
              {holds.data.map((h) => (
                <li key={h.holdId}>
                  {h.holdId} [{h.trigger}] {h.reason}
                  {h.slaBreached && ' — SLA BREACHED'}
                </li>
              ))}
            </ul>
          ))}
      </div>

      <div className="mt-3">
        <h3 className="text-xs font-medium text-neutral-400">Audit excerpt</h3>
        {audit.isError && <ErrorBox error={audit.error} />}
        {audit.isSuccess &&
          (audit.data.data.length === 0 ? (
            <p className="text-xs text-neutral-500">No admin actions on record for this account.</p>
          ) : (
            <ul className="max-h-32 space-y-0.5 overflow-y-auto text-xs text-neutral-400">
              {audit.data.data.map((a) => (
                <li key={a.id}>
                  {a.createdAt.slice(0, 19)} — {a.action} by admin #{a.adminUserId}
                </li>
              ))}
            </ul>
          ))}
      </div>
    </section>
  );
}
