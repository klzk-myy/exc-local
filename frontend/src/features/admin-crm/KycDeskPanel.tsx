/**
 * KYC desk card (Phase-10.5 Task 10.5.3.4 §4) — the account-scoped
 * slice of the officer review queue plus the tax self-certification
 * review surface (W-8BEN/W-8BEN-E/W-9 history via the audit-logged
 * admin read). The global queue stays on /admin — this card renders
 * the submissions that belong to the open customer.
 */
import { useQuery } from '@tanstack/react-query';

import type { BoundAdminApi } from '@/lib/env';
import { cardCls, ErrorBox, StatusBadge, tableCls, tdCls, thCls } from '@/lib/ui';

import { AccessDeniedCard } from '../admin/RequireAdmin';
import { isAccessDenied } from '../admin/adminRole';
import { listKycPending } from '../admin/consoles-api';
import { fetchSelfCerts } from './api';

export function KycDeskPanel({
  adminApi,
  accountId,
}: {
  adminApi: BoundAdminApi;
  accountId: number;
}) {
  const pending = useQuery({
    queryKey: ['admin-crm', 'kyc-pending', adminApi.env, accountId],
    queryFn: () => listKycPending(adminApi),
    retry: false,
    select: (all) => all.filter((s) => s.accountId === accountId),
  });
  const certs = useQuery({
    queryKey: ['admin-crm', 'self-certs', adminApi.env, accountId],
    queryFn: () => fetchSelfCerts(adminApi, accountId),
    retry: false,
  });

  if (isAccessDenied(pending.error) || isAccessDenied(certs.error)) {
    return (
      <AccessDeniedCard detail="The KYC card requires the Compliance Officer or Support Agent role." />
    );
  }

  return (
    <section className={cardCls} aria-label="KYC desk">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">KYC desk</h2>

      <h3 className="mb-1 text-xs font-semibold uppercase tracking-wide text-neutral-500">
        Pending submissions
      </h3>
      {pending.isError && <ErrorBox error={pending.error} />}
      {pending.isSuccess &&
        (pending.data.length === 0 ? (
          <p className="mb-3 text-sm text-neutral-500">No pending KYC submissions.</p>
        ) : (
          <ul className="mb-3 space-y-1 text-sm">
            {pending.data.map((s) => (
              <li key={s.id} className="flex items-center gap-2">
                <StatusBadge value={s.status} />
                <span>
                  #{s.id} → {s.requestedTier}
                  {s.jurisdiction !== '' && ` · ${s.jurisdiction}`} · SLA {s.slaDueAt.slice(0, 16)}
                </span>
              </li>
            ))}
          </ul>
        ))}

      <h3 className="mb-1 text-xs font-semibold uppercase tracking-wide text-neutral-500">
        Tax self-certifications
      </h3>
      {certs.isError && <ErrorBox error={certs.error} />}
      {certs.isSuccess &&
        (certs.data.length === 0 ? (
          <p className="text-sm text-neutral-500">No self-certifications on file.</p>
        ) : (
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Form</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>TIN country</th>
                <th className={thCls}>Filed</th>
              </tr>
            </thead>
            <tbody>
              {certs.data.map((c) => (
                <tr key={c.id}>
                  <td className={tdCls}>{c.formType}</td>
                  <td className={tdCls}>
                    <StatusBadge value={c.status || 'FILED'} />
                    {c.supersededBy !== undefined && (
                      <span className="text-xs text-neutral-500"> → #{c.supersededBy}</span>
                    )}
                  </td>
                  <td className={tdCls}>
                    {c.tinCountry ?? '—'}
                    {c.tinKind !== undefined && ` (${c.tinKind})`}
                  </td>
                  <td className={tdCls}>{c.createdAt.slice(0, 10)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ))}
    </section>
  );
}
