/**
 * Emergency-access & integrations panel — break-glass grants
 * (incident-confined ≤4h Super Admin, dual-control on the route or an
 * unreachable-approver flag), mandatory post-incident review, API-key
 * privilege-expiry extension (always a 202 dual-control submission —
 * never applied inline), and the webhook dead-letter queue with
 * retransmit.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import { apiClient } from '@/app/runtime';
import type { BoundAdminApi } from '@/lib/env';
import {
  btnGhost,
  btnPrimary,
  cardCls,
  ErrorBox,
  hintTextCls,
  inputCls,
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import {
  extendApiKeyExpiry,
  fetchDeadLetters,
  grantBreakGlass,
  retransmitDeadLetter,
  reviewBreakGlass,
} from './api';

export function EmergencyPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [grant, setGrant] = useState({
    granteeId: '',
    incidentRef: '',
    reason: '',
    ttlSeconds: '3600',
    approverMode: 'second',
    secondApproverId: '',
  });
  const [review, setReview] = useState({ id: '', notes: '' });
  const [extend, setExtend] = useState({ keyId: '', until: '', reason: '' });
  const [notice, setNotice] = useState<string | null>(null);

  const deadLetters = useQuery({
    queryKey: ['admin', 'dead-letters', adminApi.env],
    queryFn: () => fetchDeadLetters(adminApi),
    retry: false,
  });

  const invalidate = () => void qc.invalidateQueries({ queryKey: ['admin', 'dead-letters'] });
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : String(e));

  const grantMut = useMutation({
    mutationFn: () =>
      grantBreakGlass(adminApi, {
        grantee_id: Number(grant.granteeId),
        incident_ref: grant.incidentRef,
        reason: grant.reason,
        ttl_seconds: Number(grant.ttlSeconds),
        ...(grant.approverMode === 'second' && Number(grant.secondApproverId) > 0
          ? { second_approver_id: Number(grant.secondApproverId) }
          : {}),
        ...(grant.approverMode === 'unreachable' ? { unreachable_approver: true } : {}),
      }),
    onSuccess: (id) => {
      setNotice(`Break-glass grant #${id} minted (incident-confined, ≤4h).`);
      setGrant({ ...grant, reason: '', incidentRef: '' });
    },
    onError: onErr,
  });
  const reviewMut = useMutation({
    mutationFn: () => reviewBreakGlass(adminApi, Number(review.id), review.notes),
    onSuccess: () => {
      setNotice(`Grant #${review.id} marked REVIEWED.`);
      setReview({ id: '', notes: '' });
    },
    onError: onErr,
  });
  const extendMut = useMutation({
    mutationFn: () =>
      extendApiKeyExpiry(apiClient, adminApi.env, Number(extend.keyId), {
        until: extend.until,
        reason: extend.reason,
      }),
    onSuccess: (reqId) => {
      setNotice(
        `API-key expiry extension queued for a second Super Admin (request #${reqId}) — not applied inline.`,
      );
    },
    onError: onErr,
  });
  const retransmitMut = useMutation({
    mutationFn: (deliveryId: string) => retransmitDeadLetter(adminApi, deliveryId),
    onSuccess: () => {
      setNotice('Delivery requeued PENDING with a fresh attempt budget.');
      invalidate();
    },
    onError: onErr,
  });

  const denied = isAccessDenied(deadLetters.error);
  if (denied)
    return (
      <AccessDeniedCard detail="Break-glass grants require Super Admin; reviews require Risk Manager." />
    );

  return (
    <section className={cardCls} aria-label="Emergency and integrations">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">
        Break-glass, API-key expiry &amp; webhook dead letters
      </h2>
      <ErrorBox error={deadLetters.error} />
      {notice !== null && (
        <p className="mb-2 rounded border border-neutral-700 bg-neutral-950 px-2 py-1 text-xs text-neutral-300">
          {notice}
        </p>
      )}

      <h3 className="mt-2 mb-2 text-xs font-medium text-neutral-400">
        Break-glass grant — incident-confined ≤4h Super Admin
      </h3>
      <form
        className="grid gap-2 sm:grid-cols-3"
        onSubmit={(e) => {
          e.preventDefault();
          grantMut.mutate();
        }}
      >
        {(
          [
            ['granteeId', 'grantee_id'],
            ['incidentRef', 'incident_ref'],
            ['ttlSeconds', 'ttl_seconds (≤14400)'],
          ] as const
        ).map(([k, label]) => (
          <label key={k} className="block">
            <span className="sr-only">{label}</span>
            <input
              aria-label={label}
              className={inputCls}
              placeholder={label}
              value={grant[k]}
              onChange={(e) => setGrant({ ...grant, [k]: e.target.value })}
            />
          </label>
        ))}
        <label className="block sm:col-span-3">
          <span className="sr-only">Reason</span>
          <input
            aria-label="Break-glass reason"
            className={inputCls}
            placeholder="reason"
            value={grant.reason}
            onChange={(e) => setGrant({ ...grant, reason: e.target.value })}
          />
        </label>
        <label className="block">
          <span className="sr-only">Approver mode</span>
          <select
            aria-label="Approver mode"
            className={selectCls}
            value={grant.approverMode}
            onChange={(e) => setGrant({ ...grant, approverMode: e.target.value })}
          >
            <option value="second">distinct second approver</option>
            <option value="unreachable">unreachable_approver (incident)</option>
          </select>
        </label>
        {grant.approverMode === 'second' && (
          <label className="block">
            <span className="sr-only">Second approver id</span>
            <input
              aria-label="Second approver id"
              className={inputCls}
              placeholder="second_approver_id"
              value={grant.secondApproverId}
              onChange={(e) => setGrant({ ...grant, secondApproverId: e.target.value })}
            />
          </label>
        )}
        <button
          type="submit"
          className={btnPrimary}
          disabled={
            grantMut.isPending ||
            Number(grant.granteeId) <= 0 ||
            grant.incidentRef === '' ||
            grant.reason === ''
          }
        >
          Mint grant
        </button>
      </form>
      <form
        className="mt-2 grid gap-2 sm:grid-cols-4"
        onSubmit={(e) => {
          e.preventDefault();
          reviewMut.mutate();
        }}
      >
        <label className="block">
          <span className="sr-only">Grant id</span>
          <input
            aria-label="Grant id"
            className={inputCls}
            placeholder="grant id"
            value={review.id}
            onChange={(e) => setReview({ ...review, id: e.target.value })}
          />
        </label>
        <label className="block sm:col-span-2">
          <span className="sr-only">Review notes</span>
          <input
            aria-label="Review notes"
            className={inputCls}
            placeholder="post-incident review notes"
            value={review.notes}
            onChange={(e) => setReview({ ...review, notes: e.target.value })}
          />
        </label>
        <button
          type="submit"
          className={btnGhost}
          disabled={reviewMut.isPending || Number(review.id) <= 0 || review.notes === ''}
        >
          Record review
        </button>
      </form>
      <p className={hintTextCls}>
        Grantee self-review is rejected by the service — the reviewer is the incident-ops role (Risk
        Manager).
      </p>

      <h3 className="mt-5 mb-2 text-xs font-medium text-neutral-400">
        API-key expiry extension (dual-control — never inline)
      </h3>
      <form
        className="grid gap-2 sm:grid-cols-4"
        onSubmit={(e) => {
          e.preventDefault();
          extendMut.mutate();
        }}
      >
        <label className="block">
          <span className="sr-only">API key id</span>
          <input
            aria-label="API key id"
            className={inputCls}
            placeholder="api_key id"
            value={extend.keyId}
            onChange={(e) => setExtend({ ...extend, keyId: e.target.value })}
          />
        </label>
        <label className="block">
          <span className="sr-only">until</span>
          <input
            aria-label="until"
            className={inputCls}
            placeholder="until RFC3339 (≤ now+180d)"
            value={extend.until}
            onChange={(e) => setExtend({ ...extend, until: e.target.value })}
          />
        </label>
        <label className="block">
          <span className="sr-only">Extend reason</span>
          <input
            aria-label="Extend reason"
            className={inputCls}
            placeholder="reason"
            value={extend.reason}
            onChange={(e) => setExtend({ ...extend, reason: e.target.value })}
          />
        </label>
        <button
          type="submit"
          className={btnGhost}
          disabled={extendMut.isPending || Number(extend.keyId) <= 0 || extend.until === ''}
        >
          Submit extension
        </button>
      </form>

      <h3 className="mt-5 mb-2 text-xs font-medium text-neutral-400">Webhook dead letters</h3>
      <table className={tableCls}>
        <thead>
          <tr>
            <th className={thCls}>Delivery</th>
            <th className={thCls}>Account</th>
            <th className={thCls}>Event</th>
            <th className={thCls}>Status</th>
            <th className={thCls}>Attempts</th>
            <th className={thCls}>Last error</th>
            <th className={thCls}></th>
          </tr>
        </thead>
        <tbody>
          {(deadLetters.data ?? []).map((d) => (
            <tr key={d.id}>
              <td className={tdCls}>{d.deliveryId}</td>
              <td className={tdCls}>{d.accountId}</td>
              <td className={tdCls}>{d.event}</td>
              <td className={tdCls}>
                <StatusBadge value={d.status} />
              </td>
              <td className={tdCls}>
                {d.attempts}/{d.maxAttempts}
              </td>
              <td className={tdCls}>
                {d.lastStatusCode !== undefined ? `${d.lastStatusCode} ` : ''}
                {d.lastError ?? '—'}
              </td>
              <td className={tdCls}>
                <button
                  type="button"
                  className={btnGhost}
                  disabled={retransmitMut.isPending}
                  onClick={() => retransmitMut.mutate(d.deliveryId)}
                >
                  Retransmit
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {deadLetters.data?.length === 0 && (
        <p className="py-2 text-center text-xs text-neutral-500">Dead-letter queue empty.</p>
      )}
    </section>
  );
}
