/**
 * Dual-control queue panel (Task 10.3.6 / §8.2a.3) — pending request
 * list from GET /api/v1/admin/dual-control with approve/reject.
 *
 * Eligibility rules are enforced by the server: the approver must be a
 * DISTINCT admin holding the request's `required_role`. The UI reflects
 * that contract — buttons carry the required role in their labels and
 * server rejections (DUAL_CONTROL_VIOLATION) render via ErrorBox.
 */
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import { decideDualControl, fetchDualControl } from '@/lib/admin/api';
import {
  btnDanger,
  btnGhost,
  cardCls,
  tableCls,
  tdCls,
  thCls,
  ErrorBox,
  StatusBadge,
} from '@/lib/ui';
import { AccessDeniedCard } from './RequireAdmin';
import { useAdminRole, isAccessDenied } from './adminRole';

export function DualControlPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const role = useAdminRole();
  const [statusFilter, setStatusFilter] = useState('PENDING');
  const [actionError, setActionError] = useState<unknown>(null);
  const [busyId, setBusyId] = useState<number | null>(null);

  const query = useQuery({
    queryKey: ['admin', 'dual-control', adminApi.env, statusFilter],
    queryFn: () => fetchDualControl(adminApi, statusFilter === 'ALL' ? undefined : statusFilter),
    retry: false,
    refetchInterval: 15_000,
  });

  const decide = async (id: number, approve: boolean) => {
    setBusyId(id);
    setActionError(null);
    try {
      await decideDualControl(adminApi, id, approve);
      await qc.invalidateQueries({ queryKey: ['admin', 'dual-control'] });
    } catch (e) {
      setActionError(e);
    } finally {
      setBusyId(null);
    }
  };

  return (
    <section className={cardCls} aria-label="Dual control">
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-sm font-medium text-neutral-400">Dual-control queue</h2>
        <select
          aria-label="Request status filter"
          className="rounded border border-neutral-700 bg-neutral-950 px-2 py-1 text-xs"
          value={statusFilter}
          onChange={(e) => setStatusFilter(e.target.value)}
        >
          {['PENDING', 'APPROVED', 'REJECTED', 'EXPIRED', 'ALL'].map((s) => (
            <option key={s} value={s}>
              {s}
            </option>
          ))}
        </select>
      </div>
      {query.error !== null && isAccessDenied(query.error) && (
        <AccessDeniedCard detail="The dual-control queue requires a privileged admin role." />
      )}
      {query.error !== null && !isAccessDenied(query.error) && <ErrorBox error={query.error} />}
      <ErrorBox error={actionError} onDismiss={() => setActionError(null)} />
      {query.error === null && (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>#</th>
              <th className={thCls}>Operation</th>
              <th className={thCls}>Target</th>
              <th className={thCls}>Required role</th>
              <th className={thCls}>Status</th>
              <th className={thCls}>Decide</th>
            </tr>
          </thead>
          <tbody>
            {(query.data ?? []).map((r) => (
              <tr key={r.id}>
                <td className={tdCls}>{r.id}</td>
                <td className={tdCls}>{r.operation}</td>
                <td className={tdCls}>
                  {r.targetType} {r.targetId}
                </td>
                <td className={tdCls}>{r.requiredRole}</td>
                <td className={tdCls}>
                  <StatusBadge value={r.status} />
                </td>
                <td className={tdCls}>
                  {r.status === 'PENDING' && (
                    <span className="flex gap-2">
                      <button
                        type="button"
                        className={btnGhost}
                        disabled={busyId === r.id}
                        title={
                          role === r.requiredRole
                            ? 'Approve (you hold the required role — approver must differ from requester)'
                            : `Requires ${r.requiredRole} — server enforces`
                        }
                        onClick={() => void decide(r.id, true)}
                      >
                        Approve
                      </button>
                      <button
                        type="button"
                        className={btnDanger}
                        disabled={busyId === r.id}
                        onClick={() => void decide(r.id, false)}
                      >
                        Reject
                      </button>
                    </span>
                  )}
                </td>
              </tr>
            ))}
            {(query.data ?? []).length === 0 && !query.isLoading && (
              <tr>
                <td className={tdCls} colSpan={6}>
                  No {statusFilter.toLowerCase()} requests.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      )}
    </section>
  );
}
