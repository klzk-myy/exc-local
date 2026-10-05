/**
 * Quarantine panel (Phase-10.5 Task 10.5.3.7 §3) — the DepositGuard
 * suspense journal: keyset-paged rows with unmatched reason +
 * name-match score + suspense GL account, and the four-eyes
 * resolution (RELEASE_TO_CLIENT | RETURN_TO_SOURCE; approver_id must
 * differ — DUAL_CONTROL_VIOLATION server-side).
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnDanger,
  btnGhost,
  cardCls,
  ErrorBox,
  hintTextCls,
  inputCls,
  labelCls,
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import { fetchQuarantine, resolveQuarantine, type SuspenseRow } from './api';

export function QuarantinePanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [statusFilter, setStatusFilter] = useState('OPEN');
  const [acctFilter, setAcctFilter] = useState('');
  const [resolving, setResolving] = useState<SuspenseRow | null>(null);
  const [resolveForm, setResolveForm] = useState({
    action: 'RELEASE_TO_CLIENT',
    approverId: '',
    notes: '',
  });
  const [notice, setNotice] = useState<string | null>(null);

  const list = useQuery({
    queryKey: ['admin-quarantine', statusFilter, acctFilter],
    queryFn: () =>
      fetchQuarantine(adminApi, {
        status: statusFilter,
        accountId: Number(acctFilter),
      }),
  });

  const resolve = useMutation({
    mutationFn: () =>
      resolveQuarantine(adminApi, resolving?.id ?? 0, {
        action: resolveForm.action as 'RELEASE_TO_CLIENT' | 'RETURN_TO_SOURCE',
        approverId: Number(resolveForm.approverId),
        notes: resolveForm.notes === '' ? undefined : resolveForm.notes,
      }),
    onSuccess: () => {
      setNotice(`Suspense #${resolving?.id ?? ''} resolved.`);
      setResolving(null);
      void qc.invalidateQueries({ queryKey: ['admin-quarantine'] });
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Resolve failed'),
  });

  if (list.error !== null && isAccessDenied(list.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Quarantine">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Quarantine (suspense)</h2>
        <div className="flex items-end gap-2">
          <div>
            <label className={labelCls} htmlFor="q-status">
              Status
            </label>
            <select
              id="q-status"
              className={selectCls}
              value={statusFilter}
              onChange={(e) => {
                setStatusFilter(e.target.value);
              }}
            >
              <option value="">All</option>
              <option value="OPEN">OPEN</option>
              <option value="RELEASED">RELEASED</option>
              <option value="RETURNED">RETURNED</option>
            </select>
          </div>
          <div>
            <label className={labelCls} htmlFor="q-acct">
              Account
            </label>
            <input
              id="q-acct"
              className={inputCls}
              value={acctFilter}
              onChange={(e) => {
                setAcctFilter(e.target.value);
              }}
            />
          </div>
        </div>
      </div>
      <p className={hintTextCls}>
        DepositGuard suspense routing — funds sit in the 2150 suspense GL until a four-eyes
        resolution releases or returns them.
      </p>

      {list.error !== null ? <ErrorBox error={list.error} /> : null}
      {list.data?.length === 0 ? (
        <p className="mt-2 text-sm text-neutral-500">No suspense rows.</p>
      ) : null}
      {list.data !== undefined && list.data.length > 0 ? (
        <div className="mt-2 max-h-64 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Bank tx</th>
                <th className={thCls}>Account</th>
                <th className={thCls}>Amount</th>
                <th className={thCls}>Reason</th>
                <th className={thCls}>Match</th>
                <th className={thCls}>SLA</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {list.data.map((r) => (
                <tr key={r.id}>
                  <td className={tdCls}>{r.id}</td>
                  <td className={tdCls} title={r.glAccount}>
                    {r.bankTxId}
                  </td>
                  <td className={tdCls}>{r.accountId ?? '—'}</td>
                  <td className={tdCls}>
                    {r.amount} {r.currency}
                  </td>
                  <td className={tdCls}>{r.unmatchedReason}</td>
                  <td className={tdCls}>
                    {r.nameMatchScore !== undefined ? r.nameMatchScore.toFixed(2) : '—'}
                  </td>
                  <td className={tdCls}>{r.slaExpiresAt}</td>
                  <td className={tdCls}>
                    <StatusBadge value={r.quarantineStatus || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>
                    {r.quarantineStatus === 'OPEN' ? (
                      <button
                        type="button"
                        className={btnGhost}
                        onClick={() => {
                          setResolving(r);
                          setNotice(null);
                        }}
                      >
                        Resolve…
                      </button>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      {resolving !== null ? (
        <form
          aria-label="Resolve quarantine"
          className="mt-3 space-y-2 rounded border border-neutral-700 p-3"
          onSubmit={(e) => {
            e.preventDefault();
            if (Number(resolveForm.approverId) > 0) resolve.mutate();
          }}
        >
          <p className="text-sm">
            Resolve suspense <strong>#{resolving.id}</strong> ({resolving.unmatchedReason}) —
            four-eyes.
          </p>
          <div className="grid grid-cols-1 gap-2 md:grid-cols-3">
            <select
              aria-label="Resolution action"
              className={selectCls}
              value={resolveForm.action}
              onChange={(e) => {
                setResolveForm({ ...resolveForm, action: e.target.value });
              }}
            >
              <option value="RELEASE_TO_CLIENT">RELEASE_TO_CLIENT</option>
              <option value="RETURN_TO_SOURCE">RETURN_TO_SOURCE</option>
            </select>
            <input
              aria-label="Approver id"
              className={inputCls}
              placeholder="approver_id (≠ you)"
              value={resolveForm.approverId}
              onChange={(e) => {
                setResolveForm({ ...resolveForm, approverId: e.target.value });
              }}
            />
            <input
              aria-label="Resolution notes"
              className={inputCls}
              placeholder="notes"
              value={resolveForm.notes}
              onChange={(e) => {
                setResolveForm({ ...resolveForm, notes: e.target.value });
              }}
            />
          </div>
          <div className="flex gap-2">
            <button type="submit" className={btnDanger} disabled={resolve.isPending}>
              Apply resolution
            </button>
            <button type="button" className={btnGhost} onClick={() => setResolving(null)}>
              Cancel
            </button>
          </div>
        </form>
      ) : null}
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
