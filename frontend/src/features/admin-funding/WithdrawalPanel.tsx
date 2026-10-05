/**
 * Withdrawal-approvals panel (Phase-10.5 Task 10.5.3.7 §2) — the
 * actionable leg of the >$50K PENDING_REVIEW tier. Approve/reject is
 * four-eyes (approver_id must differ from the acting admin — the
 * route is registry-DualControl); the returned WithdrawalResult
 * carries the review tier + risk flags (whitelist/beneficiary/velocity
 * context is server-side, surfaced via flags).
 */
import { useMutation } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnDanger,
  btnPrimary,
  cardCls,
  hintTextCls,
  inputCls,
  labelCls,
  StatusBadge,
} from '@/lib/ui';

import { reviewWithdrawal, type WithdrawalResult } from './api';
import { TierLegend } from './DepositOpsPanel';

export function WithdrawalPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [form, setForm] = useState({ id: '', approverId: '', note: '' });
  const [result, setResult] = useState<WithdrawalResult | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const act = useMutation({
    mutationFn: (approve: boolean) =>
      reviewWithdrawal(adminApi, Number(form.id), approve, {
        approverId: Number(form.approverId),
        note: form.note === '' ? undefined : form.note,
      }),
    onSuccess: (r) => {
      setResult(r);
      setNotice(null);
    },
    onError: (e) => {
      setResult(null);
      setNotice(e instanceof Error ? e.message : 'Review failed');
    },
  });

  const valid = Number(form.id) > 0 && Number(form.approverId) > 0;

  return (
    <section className={cardCls} aria-label="Withdrawal approvals">
      <h2 className="mb-1 text-sm font-semibold">Withdrawal approvals</h2>
      <TierLegend />
      <p className={hintTextCls}>
        Pending withdrawals surface via the ops-alerts rail; enter the id to decide (four-eyes).
      </p>

      <form
        aria-label="Withdrawal decision"
        className="mt-3 space-y-2"
        onSubmit={(e) => e.preventDefault()}
      >
        <div className="grid grid-cols-1 gap-2 md:grid-cols-3">
          <div>
            <label className={labelCls} htmlFor="wd-id">
              Withdrawal id
            </label>
            <input
              id="wd-id"
              className={inputCls}
              value={form.id}
              onChange={(e) => {
                setForm({ ...form, id: e.target.value });
              }}
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="wd-app">
              Approver id (≠ you)
            </label>
            <input
              id="wd-app"
              className={inputCls}
              value={form.approverId}
              onChange={(e) => {
                setForm({ ...form, approverId: e.target.value });
              }}
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="wd-note">
              Note
            </label>
            <input
              id="wd-note"
              className={inputCls}
              value={form.note}
              onChange={(e) => {
                setForm({ ...form, note: e.target.value });
              }}
            />
          </div>
        </div>
        <div className="flex gap-2">
          <button
            type="button"
            className={btnPrimary}
            disabled={act.isPending || !valid}
            onClick={() => act.mutate(true)}
          >
            Approve
          </button>
          <button
            type="button"
            className={btnDanger}
            disabled={act.isPending || !valid}
            onClick={() => act.mutate(false)}
          >
            Reject
          </button>
        </div>
      </form>

      {result !== null ? (
        <div
          aria-label="Withdrawal result"
          className="mt-3 rounded border border-neutral-700 p-2 text-sm"
        >
          <div className="flex flex-wrap items-center gap-2">
            <span>#{result.withdrawalId}</span>
            <StatusBadge value={result.status || 'UNKNOWN'} />
            {result.reviewTier !== undefined ? <StatusBadge value={result.reviewTier} /> : null}
            <span className={hintTextCls}>
              {result.amount} {result.currency}
              {result.usdAmount !== undefined ? ` (~$${result.usdAmount})` : ''}
            </span>
          </div>
          {result.flags.length > 0 ? (
            <p className="mt-1 text-xs text-amber-300">flags: {result.flags.join(', ')}</p>
          ) : null}
        </div>
      ) : null}
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
