/**
 * Enforcement panel (Phase-10.5 Task 10.5.3.5 §5) — the
 * WARN→THROTTLE→RESTRICT→SUSPEND ladder plus DISMISS, anchored to a
 * surveillance signal id. The order-gate effect of the selected rung
 * is previewed before submission so the officer sees what the gateway
 * will do to order flow. Actions land in the enforcement ledger
 * (GET /admin/enforcement) and execute through the compliance
 * service's hold/kill-switch/throttle seams — never the engine.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnGhost,
  btnPrimary,
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
import { ENFORCE_ACTIONS, enforce, fetchEnforcement } from './api';

/** Order-gate preview per ladder rung. */
const ACTION_EFFECT: Record<string, string> = {
  WARN: 'No order-gate change — records a warning on the signal.',
  THROTTLE: 'Order gateway rate-limits the account (params set the cap) until the TTL expires.',
  RESTRICT: 'New orders rejected at the order gate; cancels remain possible. TTL-bounded.',
  SUSPEND: 'Full trading suspension — order entry blocked until an officer lifts the action.',
  DISMISS: 'Closes the signal with no enforcement — no gate effect.',
};

const LADDER = ['WARN', 'THROTTLE', 'RESTRICT', 'SUSPEND'] as const;

export function EnforcementPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [accountFilter, setAccountFilter] = useState('');
  const [notice, setNotice] = useState<string | null>(null);
  const [form, setForm] = useState({ signalId: '', action: 'WARN', note: '', ttl: '' });

  const list = useQuery({
    queryKey: ['admin-enforcement', accountFilter, adminApi.env],
    queryFn: () => fetchEnforcement(adminApi, Number(accountFilter)),
  });

  const act = useMutation({
    mutationFn: () =>
      enforce(adminApi, Number(form.signalId), {
        action: form.action,
        note: form.note,
        ttlSeconds: form.ttl === '' ? undefined : Number(form.ttl),
      }),
    onSuccess: () => {
      setNotice(`${form.action} recorded on signal ${form.signalId}.`);
      void qc.invalidateQueries({ queryKey: ['admin-enforcement'] });
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Enforce failed'),
  });

  if (list.error !== null && isAccessDenied(list.error)) {
    return <AccessDeniedCard />;
  }

  const ladderIndex = LADDER.indexOf(form.action as (typeof LADDER)[number]);

  return (
    <section className={cardCls} aria-label="Enforcement">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Enforcement</h2>
        <div className="flex items-end gap-2">
          <div>
            <label className={labelCls} htmlFor="ef-acct">
              Account filter
            </label>
            <input
              id="ef-acct"
              className={inputCls}
              value={accountFilter}
              onChange={(e) => {
                setAccountFilter(e.target.value);
              }}
            />
          </div>
          <button
            type="button"
            className={btnGhost}
            onClick={() => void list.refetch()}
            disabled={list.isFetching}
          >
            Refresh
          </button>
        </div>
      </div>

      <div className="mb-3 flex flex-wrap items-center gap-1" aria-label="Enforcement ladder">
        {LADDER.map((rung, i) => (
          <span key={rung} className="flex items-center gap-1 text-xs">
            <StatusBadge value={rung} />
            {i < LADDER.length - 1 ? <span className="text-neutral-600">→</span> : null}
          </span>
        ))}
        <span className={`${hintTextCls} ml-2`}>
          escalation ladder; DISMISS closes without action
        </span>
      </div>

      {list.error !== null ? <ErrorBox error={list.error} /> : null}
      {list.data?.length === 0 ? (
        <p className="text-sm text-neutral-500">No enforcement actions.</p>
      ) : null}
      {list.data !== undefined && list.data.length > 0 ? (
        <div className="max-h-56 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Action</th>
                <th className={thCls}>Signal</th>
                <th className={thCls}>Account</th>
                <th className={thCls}>Source</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Actor</th>
                <th className={thCls}>Expires</th>
                <th className={thCls}>At</th>
              </tr>
            </thead>
            <tbody>
              {list.data.map((a) => (
                <tr key={a.actionId}>
                  <td className={tdCls}>{a.action}</td>
                  <td className={tdCls}>{a.signalId ?? '—'}</td>
                  <td className={tdCls}>{a.accountId ?? '—'}</td>
                  <td className={tdCls}>{a.source}</td>
                  <td className={tdCls}>
                    <StatusBadge value={a.status || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>{a.actorId}</td>
                  <td className={tdCls}>{a.expiresAt ?? '—'}</td>
                  <td className={tdCls}>{a.createdAt}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      <form
        aria-label="Enforce"
        className="mt-4 space-y-2 border-t border-neutral-800 pt-3"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(form.signalId) > 0 && form.note !== '') act.mutate();
        }}
      >
        <div className="grid grid-cols-1 gap-2 md:grid-cols-4">
          <div>
            <label className={labelCls} htmlFor="ef-sig">
              Signal id
            </label>
            <input
              id="ef-sig"
              className={inputCls}
              value={form.signalId}
              onChange={(e) => {
                setForm({ ...form, signalId: e.target.value });
              }}
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="ef-action">
              Action
            </label>
            <select
              id="ef-action"
              className={selectCls}
              value={form.action}
              onChange={(e) => {
                setForm({ ...form, action: e.target.value });
              }}
            >
              {ENFORCE_ACTIONS.map((a) => (
                <option key={a} value={a}>
                  {a}
                </option>
              ))}
            </select>
          </div>
          <div>
            <label className={labelCls} htmlFor="ef-ttl">
              TTL seconds
            </label>
            <input
              id="ef-ttl"
              className={inputCls}
              placeholder="0 = indefinite"
              value={form.ttl}
              onChange={(e) => {
                setForm({ ...form, ttl: e.target.value });
              }}
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="ef-note">
              Note
            </label>
            <input
              id="ef-note"
              className={inputCls}
              value={form.note}
              onChange={(e) => {
                setForm({ ...form, note: e.target.value });
              }}
            />
          </div>
        </div>
        <p className="text-sm text-amber-300" aria-label="Order-gate effect preview">
          {ACTION_EFFECT[form.action] ?? '—'}
          {ladderIndex >= 0 ? ` (rung ${ladderIndex + 1} of ${LADDER.length})` : ''}
        </p>
        <button type="submit" className={btnPrimary} disabled={act.isPending}>
          Apply enforcement
        </button>
        {notice !== null ? <p className="text-sm">{notice}</p> : null}
      </form>
    </section>
  );
}
