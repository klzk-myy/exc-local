/**
 * Financial-promotions panel (Task 21.3.26) — the approval workflow
 * DRAFT → PENDING_REVIEW → APPROVED|REJECTED → WITHDRAWN plus the
 * marketing report. Approving a claims-carrying promotion requires a
 * distinct `second_approver_id` (claims route gate); the checklist must
 * assert all five MiFID promo elements.
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
  hintTextCls,
  inputCls,
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { apiClient } from '@/app/runtime';
import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import {
  approvePromotion,
  createPromotion,
  fetchPromoReport,
  fetchPromotions,
  promotionTransition,
  revisePromotion,
} from './api';

const CHANNELS = ['LANDING', 'AD', 'EMAIL', 'PUSH', 'SOCIAL', 'IN_APP'];
const STATUSES = ['', 'DRAFT', 'PENDING_REVIEW', 'APPROVED', 'REJECTED', 'WITHDRAWN', 'EXPIRED'];

const CHECKLIST_KEYS = [
  ['risk_warning', 'Risk warning'],
  ['capital_at_risk', 'Capital at risk'],
  ['claim_basis', 'Claim basis'],
  ['entity_details', 'Entity details'],
  ['fair_clear', 'Fair & clear'],
] as const;

export function PromotionsPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [filter, setFilter] = useState('');
  const [revise, setRevise] = useState(false);
  const [form, setForm] = useState({
    slug: '',
    channel: 'LANDING',
    bodyRef: '',
    title: '',
    containsClaim: 'false',
  });
  const [approval, setApproval] = useState<Record<string, string | boolean>>({});
  const [rejecting, setRejecting] = useState<string | null>(null);
  const [rejectReason, setRejectReason] = useState('');
  const [notice, setNotice] = useState<string | null>(null);

  const promos = useQuery({
    queryKey: ['admin', 'promotions', adminApi.env, filter],
    queryFn: () => fetchPromotions(adminApi, filter),
    retry: false,
  });
  const report = useQuery({
    queryKey: ['admin', 'promo-report', adminApi.env],
    queryFn: () => fetchPromoReport(adminApi),
    retry: false,
  });

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin', 'promotions'] });
    void qc.invalidateQueries({ queryKey: ['admin', 'promo-report'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : String(e));

  const createMut = useMutation({
    mutationFn: () =>
      revise
        ? revisePromotion(apiClient, adminApi.env, {
            slug: form.slug,
            channel: form.channel,
            body_ref: form.bodyRef,
            title: form.title,
            contains_claim: form.containsClaim === 'true',
          })
        : createPromotion(adminApi, {
            slug: form.slug,
            channel: form.channel,
            body_ref: form.bodyRef,
            title: form.title,
            contains_claim: form.containsClaim === 'true',
          }),
    onSuccess: () => {
      setNotice(revise ? 'New version landed under the slug.' : 'Promotion created (DRAFT).');
      invalidate();
    },
    onError: onErr,
  });
  const verbMut = useMutation({
    mutationFn: ({
      id,
      verb,
      reason,
    }: {
      id: number;
      verb: 'submit' | 'reject' | 'withdraw';
      reason?: string;
    }) => promotionTransition(adminApi, id, verb, reason !== undefined ? { reason } : {}),
    onSuccess: (_d, v) => {
      setNotice(`Promotion #${v.id} → ${v.verb} done.`);
      invalidate();
    },
    onError: onErr,
  });
  const approveMut = useMutation({
    mutationFn: () => {
      const id = Number(approval['id'] ?? '0');
      const checklist = {
        risk_warning: approval['risk_warning'] === true,
        capital_at_risk: approval['capital_at_risk'] === true,
        claim_basis: approval['claim_basis'] === true,
        entity_details: approval['entity_details'] === true,
        fair_clear: approval['fair_clear'] === true,
      };
      return approvePromotion(adminApi, id, {
        checklist,
        ...(Number(approval['second_approver_id'] ?? '0') > 0
          ? { second_approver_id: Number(approval['second_approver_id']) }
          : {}),
        ...((approval['approved_until'] ?? '') !== ''
          ? { approved_until: String(approval['approved_until']) }
          : {}),
      });
    },
    onSuccess: () => {
      setNotice('Promotion APPROVED.');
      setApproval({});
      invalidate();
    },
    onError: onErr,
  });

  const denied = isAccessDenied(promos.error);
  if (denied)
    return <AccessDeniedCard detail="Financial promotions require a Compliance Officer role." />;

  return (
    <section className={cardCls} aria-label="Promotions">
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-sm font-medium text-neutral-400">Financial promotions</h2>
        <select
          aria-label="Status filter"
          className={selectCls}
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        >
          {STATUSES.map((s) => (
            <option key={s} value={s}>
              {s === '' ? 'all' : s}
            </option>
          ))}
        </select>
      </div>
      <ErrorBox error={promos.error ?? report.error} />
      {report.data && (
        <div className="mb-2 flex flex-wrap gap-3 text-xs text-neutral-400">
          <span>
            by status:{' '}
            {Object.entries(report.data.byStatus)
              .map(([k, v]) => `${k}:${v}`)
              .join(' ') || '—'}
          </span>
          <span>SLA breached {report.data.slaBreached}</span>
          <span>drafts aging {report.data.draftsAging}</span>
          <span>expiring ≤30d {report.data.expiringIn30d}</span>
          {report.data.expiredFlagged > 0 && (
            <span className="text-amber-400">
              {report.data.expiredFlagged} APPROVED past approved_until (cache flagged)
            </span>
          )}
        </div>
      )}
      {notice !== null && (
        <p className="mb-2 rounded border border-neutral-700 bg-neutral-950 px-2 py-1 text-xs text-neutral-300">
          {notice}
        </p>
      )}
      <table className={tableCls}>
        <thead>
          <tr>
            <th className={thCls}>ID</th>
            <th className={thCls}>Slug</th>
            <th className={thCls}>v</th>
            <th className={thCls}>Channel</th>
            <th className={thCls}>Title</th>
            <th className={thCls}>Status</th>
            <th className={thCls}>Claim</th>
            <th className={thCls}>Until</th>
            <th className={thCls}></th>
          </tr>
        </thead>
        <tbody>
          {(promos.data ?? []).map((p) => (
            <tr key={p.promotionId}>
              <td className={tdCls}>{p.promotionId}</td>
              <td className={tdCls}>{p.slug}</td>
              <td className={tdCls}>{p.version}</td>
              <td className={tdCls}>{p.channel}</td>
              <td className={tdCls}>{p.title}</td>
              <td className={tdCls}>
                <StatusBadge value={p.approvalStatus} />
                {p.rejectionReason !== undefined && (
                  <span className="ml-1 text-xs text-red-400">{p.rejectionReason}</span>
                )}
              </td>
              <td className={tdCls}>{p.containsClaim ? 'yes' : 'no'}</td>
              <td className={tdCls}>{p.approvedUntil?.slice(0, 10) ?? '—'}</td>
              <td className={tdCls}>
                <div className="flex flex-wrap gap-1">
                  {p.approvalStatus === 'DRAFT' && (
                    <button
                      type="button"
                      className={btnGhost}
                      onClick={() => verbMut.mutate({ id: p.promotionId, verb: 'submit' })}
                    >
                      Submit
                    </button>
                  )}
                  {p.approvalStatus === 'PENDING_REVIEW' && (
                    <>
                      <button
                        type="button"
                        className={btnPrimary}
                        onClick={() =>
                          setApproval({
                            id: String(p.promotionId),
                            risk_warning: true,
                            capital_at_risk: true,
                            claim_basis: true,
                            entity_details: true,
                            fair_clear: true,
                          })
                        }
                      >
                        Approve
                      </button>
                      <button
                        type="button"
                        className={btnDanger}
                        onClick={() => setRejecting(String(p.promotionId))}
                      >
                        Reject
                      </button>
                    </>
                  )}
                  {p.approvalStatus === 'APPROVED' && (
                    <button
                      type="button"
                      className={btnGhost}
                      onClick={() => verbMut.mutate({ id: p.promotionId, verb: 'withdraw' })}
                    >
                      Withdraw
                    </button>
                  )}
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {rejecting !== null && (
        <form
          className="mt-3 flex gap-2 rounded border border-red-900/60 p-3"
          onSubmit={(e) => {
            e.preventDefault();
            verbMut.mutate({ id: Number(rejecting), verb: 'reject', reason: rejectReason });
            setRejecting(null);
            setRejectReason('');
          }}
        >
          <span className="self-center text-xs text-neutral-400">Reject #{rejecting}</span>
          <label className="block flex-1">
            <span className="sr-only">Rejection reason</span>
            <input
              aria-label="Rejection reason"
              className={inputCls}
              placeholder="reason (required)"
              value={rejectReason}
              onChange={(e) => setRejectReason(e.target.value)}
            />
          </label>
          <button type="submit" className={btnDanger} disabled={rejectReason === ''}>
            Confirm reject
          </button>
        </form>
      )}
      {approval['id'] !== undefined && (
        <form
          className="mt-3 grid gap-2 rounded border border-sky-900/60 p-3 sm:grid-cols-3"
          onSubmit={(e) => {
            e.preventDefault();
            approveMut.mutate();
          }}
        >
          <span className="self-center text-xs text-neutral-400">
            Approve #{approval['id']} — checklist (all asserted):
          </span>
          {CHECKLIST_KEYS.map(([k, label]) => (
            <label key={k} className="flex items-center gap-1 text-xs text-neutral-300">
              <input
                type="checkbox"
                aria-label={label}
                checked={approval[k] === true}
                onChange={(e) => setApproval({ ...approval, [k]: e.target.checked })}
              />
              {label}
            </label>
          ))}
          <label className="block">
            <span className="sr-only">Second approver id</span>
            <input
              aria-label="Second approver id"
              className={inputCls}
              placeholder="second_approver_id (claims promos)"
              value={String(approval['second_approver_id'] ?? '')}
              onChange={(e) => setApproval({ ...approval, second_approver_id: e.target.value })}
            />
          </label>
          <label className="block">
            <span className="sr-only">approved_until</span>
            <input
              aria-label="approved_until"
              className={inputCls}
              placeholder="approved_until RFC3339 (≤12mo)"
              value={String(approval['approved_until'] ?? '')}
              onChange={(e) => setApproval({ ...approval, approved_until: e.target.value })}
            />
          </label>
          <button type="submit" className={btnPrimary} disabled={approveMut.isPending}>
            Confirm approval
          </button>
        </form>
      )}
      <form
        className="mt-3 grid gap-2 sm:grid-cols-6"
        onSubmit={(e) => {
          e.preventDefault();
          createMut.mutate();
        }}
      >
        <label className="block">
          <span className="sr-only">Slug</span>
          <input
            aria-label="Slug"
            className={inputCls}
            placeholder="slug"
            value={form.slug}
            onChange={(e) => setForm({ ...form, slug: e.target.value })}
          />
        </label>
        <label className="block">
          <span className="sr-only">Channel</span>
          <select
            aria-label="Channel"
            className={selectCls}
            value={form.channel}
            onChange={(e) => setForm({ ...form, channel: e.target.value })}
          >
            {CHANNELS.map((c) => (
              <option key={c} value={c}>
                {c}
              </option>
            ))}
          </select>
        </label>
        <label className="block">
          <span className="sr-only">Title</span>
          <input
            aria-label="Promo title"
            className={inputCls}
            placeholder="title"
            value={form.title}
            onChange={(e) => setForm({ ...form, title: e.target.value })}
          />
        </label>
        <label className="block">
          <span className="sr-only">body_ref</span>
          <input
            aria-label="body_ref"
            className={inputCls}
            placeholder="body_ref"
            value={form.bodyRef}
            onChange={(e) => setForm({ ...form, bodyRef: e.target.value })}
          />
        </label>
        <label className="block">
          <span className="sr-only">Contains claim</span>
          <select
            aria-label="Contains claim"
            className={selectCls}
            value={form.containsClaim}
            onChange={(e) => setForm({ ...form, containsClaim: e.target.value })}
          >
            <option value="false">no claim</option>
            <option value="true">contains claim</option>
          </select>
        </label>
        <label className="flex items-center gap-1 text-xs text-neutral-300">
          <input
            type="checkbox"
            aria-label="Revise existing"
            checked={revise}
            onChange={(e) => setRevise(e.target.checked)}
          />
          revise (new version)
        </label>
        <button
          type="submit"
          className={btnGhost}
          disabled={createMut.isPending || form.slug === '' || form.bodyRef === ''}
        >
          {revise ? 'Revise (PUT)' : 'Create (DRAFT)'}
        </button>
      </form>
      <p className={hintTextCls}>
        PUT re-uses the slug and lands a new version row — the prior approval chain is kept. Claims
        promotions require a distinct second approver; approval validity is bounded to 12 months.
      </p>
    </section>
  );
}
