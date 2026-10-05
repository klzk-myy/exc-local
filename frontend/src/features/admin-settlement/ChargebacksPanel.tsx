/**
 * Chargebacks panel (Phase-10.5 Task 10.5.3.9 §5) — card-dispute
 * register: open a case (four-eyes approver_user_id + optional
 * account freeze), the OPEN → SUBMITTED → WON|LOST lifecycle, and
 * sha256-hashed evidence detail.
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
import { createChargeback, fetchChargebacks, resolveChargeback, submitChargeback } from './api';

export function ChargebacksPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [statusFilter, setStatusFilter] = useState('');
  const [form, setForm] = useState({
    accountId: '',
    network: 'VISA',
    currency: 'USD',
    amount: '',
    reason: '',
    freeze: false,
    approverId: '',
  });
  const [resolveForm, setResolveForm] = useState({ id: '', outcome: 'WON', note: '' });
  const [notice, setNotice] = useState<string | null>(null);

  const list = useQuery({
    queryKey: ['admin-chargebacks', statusFilter],
    queryFn: () =>
      fetchChargebacks(adminApi, { status: statusFilter === '' ? undefined : statusFilter }),
  });

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-chargebacks'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');
  const create = useMutation({
    mutationFn: () =>
      createChargeback(adminApi, {
        accountId: Number(form.accountId),
        cardNetwork: form.network,
        currency: form.currency,
        amount: form.amount,
        reason: form.reason,
        freezeAccount: form.freeze,
        approverUserId: Number(form.approverId),
      }),
    onSuccess: () => {
      setNotice('Chargeback case opened.');
      invalidate();
    },
    onError: onErr,
  });
  const submit = useMutation({
    mutationFn: (id: number) => submitChargeback(adminApi, id),
    onSuccess: invalidate,
    onError: onErr,
  });
  const resolve = useMutation({
    mutationFn: () =>
      resolveChargeback(adminApi, Number(resolveForm.id), {
        outcome: resolveForm.outcome as 'WON' | 'LOST',
        note: resolveForm.note,
      }),
    onSuccess: () => {
      setNotice(`Chargeback #${resolveForm.id} resolved ${resolveForm.outcome}.`);
      invalidate();
    },
    onError: onErr,
  });

  if (list.error !== null && isAccessDenied(list.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Chargebacks">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Chargebacks</h2>
        <div>
          <label className={labelCls} htmlFor="cb-status">
            Status
          </label>
          <select
            id="cb-status"
            className={selectCls}
            value={statusFilter}
            onChange={(e) => {
              setStatusFilter(e.target.value);
            }}
          >
            <option value="">All</option>
            <option value="OPEN">OPEN</option>
            <option value="SUBMITTED">SUBMITTED</option>
            <option value="WON">WON</option>
            <option value="LOST">LOST</option>
          </select>
        </div>
      </div>

      {list.error !== null ? <ErrorBox error={list.error} /> : null}
      {list.data?.length === 0 ? (
        <p className="text-sm text-neutral-500">No chargeback cases.</p>
      ) : null}
      {list.data !== undefined && list.data.length > 0 ? (
        <div className="max-h-48 overflow-y-auto" tabIndex={0}>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Account</th>
                <th className={thCls}>Amount</th>
                <th className={thCls}>Network</th>
                <th className={thCls}>Reason</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {list.data.map((c) => (
                <tr key={c.id}>
                  <td className={tdCls}>{c.id}</td>
                  <td className={tdCls}>{c.accountId}</td>
                  <td className={tdCls}>
                    {c.amount} {c.currency}
                  </td>
                  <td className={tdCls}>{c.cardNetwork ?? '—'}</td>
                  <td className={tdCls}>{c.reason}</td>
                  <td className={tdCls}>
                    <StatusBadge value={c.status || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>
                    {c.status === 'OPEN' ? (
                      <button
                        type="button"
                        className={btnGhost}
                        disabled={submit.isPending}
                        onClick={() => {
                          submit.mutate(c.id);
                        }}
                      >
                        Submit
                      </button>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      <form
        aria-label="Open chargeback"
        className="mt-3 grid grid-cols-2 gap-2 md:grid-cols-4"
        onSubmit={(e) => {
          e.preventDefault();
          if (
            Number(form.accountId) > 0 &&
            form.amount !== '' &&
            form.reason !== '' &&
            Number(form.approverId) > 0
          ) {
            create.mutate();
          }
        }}
      >
        <input
          aria-label="Chargeback account"
          className={inputCls}
          placeholder="account_id"
          value={form.accountId}
          onChange={(e) => {
            setForm({ ...form, accountId: e.target.value });
          }}
        />
        <select
          aria-label="Card network"
          className={selectCls}
          value={form.network}
          onChange={(e) => {
            setForm({ ...form, network: e.target.value });
          }}
        >
          <option value="VISA">VISA</option>
          <option value="MASTERCARD">MASTERCARD</option>
          <option value="AMEX">AMEX</option>
        </select>
        <input
          aria-label="Chargeback amount"
          className={inputCls}
          placeholder="amount"
          value={form.amount}
          onChange={(e) => {
            setForm({ ...form, amount: e.target.value });
          }}
        />
        <input
          aria-label="Chargeback reason"
          className={inputCls}
          placeholder="reason"
          value={form.reason}
          onChange={(e) => {
            setForm({ ...form, reason: e.target.value });
          }}
        />
        <input
          aria-label="Approver user id"
          className={inputCls}
          placeholder="approver_user_id (4-eyes)"
          value={form.approverId}
          onChange={(e) => {
            setForm({ ...form, approverId: e.target.value });
          }}
        />
        <label className="flex items-center gap-1 text-sm">
          <input
            type="checkbox"
            checked={form.freeze}
            onChange={(e) => {
              setForm({ ...form, freeze: e.target.checked });
            }}
          />
          Freeze account
        </label>
        <button type="submit" className={btnPrimary} disabled={create.isPending}>
          Open case
        </button>
      </form>

      <form
        aria-label="Resolve chargeback"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(resolveForm.id) > 0 && resolveForm.note !== '') resolve.mutate();
        }}
      >
        <input
          aria-label="Resolve chargeback id"
          className={inputCls}
          placeholder="chargeback_id"
          value={resolveForm.id}
          onChange={(e) => {
            setResolveForm({ ...resolveForm, id: e.target.value });
          }}
        />
        <select
          aria-label="Outcome"
          className={selectCls}
          value={resolveForm.outcome}
          onChange={(e) => {
            setResolveForm({ ...resolveForm, outcome: e.target.value });
          }}
        >
          <option value="WON">WON</option>
          <option value="LOST">LOST</option>
        </select>
        <input
          aria-label="Resolution note"
          className={inputCls}
          placeholder="note"
          value={resolveForm.note}
          onChange={(e) => {
            setResolveForm({ ...resolveForm, note: e.target.value });
          }}
        />
        <button type="submit" className={btnDanger} disabled={resolve.isPending}>
          Resolve
        </button>
      </form>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
