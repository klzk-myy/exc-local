/**
 * Bank-accounts panel (Phase-10.5 Task 10.5.3.7 §4) — the beneficiary
 * registry verification queue. Verify is four-eyes ({approver_id,
 * method}); reject is single-approver ({reason} — releases no funds).
 * The unlock_at timestamp is the withdrawal-activation evidence.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnDanger,
  btnGhost,
  btnPrimary,
  cardCls,
  ErrorBox,
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
import {
  fetchBankAccounts,
  rejectBankAccount,
  verifyBankAccount,
  type BankAccountRow,
} from './api';

export function BankAccountsPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [statusFilter, setStatusFilter] = useState('PENDING_VERIFICATION');
  const [acting, setActing] = useState<{ row: BankAccountRow; kind: 'verify' | 'reject' } | null>(
    null,
  );
  const [approverId, setApproverId] = useState('');
  const [method, setMethod] = useState('BANK_STATEMENT');
  const [reason, setReason] = useState('');
  const [notice, setNotice] = useState<string | null>(null);

  const list = useQuery({
    queryKey: ['admin-bank-accounts', statusFilter],
    queryFn: () => fetchBankAccounts(adminApi, statusFilter),
  });

  const done = (msg: string) => {
    setNotice(msg);
    setActing(null);
    void qc.invalidateQueries({ queryKey: ['admin-bank-accounts'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const verify = useMutation({
    mutationFn: () =>
      verifyBankAccount(adminApi, acting?.row.bankAccountId ?? 0, {
        approverId: Number(approverId),
        method,
      }),
    onSuccess: () => done('Beneficiary verified.'),
    onError: onErr,
  });
  const reject = useMutation({
    mutationFn: () => rejectBankAccount(adminApi, acting?.row.bankAccountId ?? 0, reason),
    onSuccess: () => done('Beneficiary rejected.'),
    onError: onErr,
  });

  if (list.error !== null && isAccessDenied(list.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Bank accounts">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Beneficiary bank accounts</h2>
        <div>
          <label className={labelCls} htmlFor="ba-status">
            Status
          </label>
          <select
            id="ba-status"
            className={selectCls}
            value={statusFilter}
            onChange={(e) => {
              setStatusFilter(e.target.value);
            }}
          >
            <option value="">All</option>
            <option value="PENDING_VERIFICATION">PENDING_VERIFICATION</option>
            <option value="VERIFIED">VERIFIED</option>
            <option value="REJECTED">REJECTED</option>
          </select>
        </div>
      </div>

      {list.error !== null ? <ErrorBox error={list.error} /> : null}
      {list.data?.length === 0 ? (
        <p className="text-sm text-neutral-500">No beneficiary registrations.</p>
      ) : null}
      {list.data !== undefined && list.data.length > 0 ? (
        <div className="max-h-56 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Account</th>
                <th className={thCls}>Beneficiary</th>
                <th className={thCls}>Bank</th>
                <th className={thCls}>IBAN</th>
                <th className={thCls}>Rail</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Unlocks</th>
                <th className={thCls}>
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {list.data.map((r) => (
                <tr key={r.bankAccountId}>
                  <td className={tdCls}>{r.bankAccountId}</td>
                  <td className={tdCls}>{r.accountId}</td>
                  <td className={tdCls}>{r.beneficiaryName}</td>
                  <td className={tdCls}>{r.bankName}</td>
                  <td className={tdCls}>{r.iban ?? '—'}</td>
                  <td className={tdCls}>{r.rail}</td>
                  <td className={tdCls}>
                    <StatusBadge value={r.status || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>{r.unlockedAt ?? '—'}</td>
                  <td className={tdCls}>
                    {r.status === 'PENDING_VERIFICATION' ? (
                      <div className="flex gap-1">
                        <button
                          type="button"
                          className={btnGhost}
                          onClick={() => {
                            setActing({ row: r, kind: 'verify' });
                            setNotice(null);
                          }}
                        >
                          Verify…
                        </button>
                        <button
                          type="button"
                          className={btnGhost}
                          onClick={() => {
                            setActing({ row: r, kind: 'reject' });
                            setNotice(null);
                          }}
                        >
                          Reject…
                        </button>
                      </div>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      {acting !== null ? (
        <form
          aria-label={`${acting.kind} beneficiary`}
          className="mt-3 space-y-2 rounded border border-neutral-700 p-3"
          onSubmit={(e) => {
            e.preventDefault();
            if (acting.kind === 'verify') {
              if (Number(approverId) > 0) verify.mutate();
            } else if (reason !== '') {
              reject.mutate();
            }
          }}
        >
          <p className="text-sm">
            {acting.kind === 'verify' ? 'Verify' : 'Reject'} beneficiary{' '}
            <strong>#{acting.row.bankAccountId}</strong> ({acting.row.beneficiaryName} —{' '}
            {acting.row.bankName})
          </p>
          {acting.kind === 'verify' ? (
            <div className="flex gap-2">
              <input
                aria-label="Approver id"
                className={inputCls}
                placeholder="approver_id (≠ you)"
                value={approverId}
                onChange={(e) => {
                  setApproverId(e.target.value);
                }}
              />
              <select
                aria-label="Verification method"
                className={selectCls}
                value={method}
                onChange={(e) => {
                  setMethod(e.target.value);
                }}
              >
                <option value="BANK_STATEMENT">BANK_STATEMENT</option>
                <option value="MICRO_DEPOSIT">MICRO_DEPOSIT</option>
              </select>
            </div>
          ) : (
            <input
              aria-label="Reject reason"
              className={inputCls}
              placeholder="reason (required)"
              value={reason}
              onChange={(e) => {
                setReason(e.target.value);
              }}
            />
          )}
          <div className="flex gap-2">
            <button
              type="submit"
              className={acting.kind === 'verify' ? btnPrimary : btnDanger}
              disabled={verify.isPending || reject.isPending}
            >
              {acting.kind === 'verify' ? 'Verify (4-eyes)' : 'Reject'}
            </button>
            <button type="button" className={btnGhost} onClick={() => setActing(null)}>
              Cancel
            </button>
          </div>
        </form>
      ) : null}
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
