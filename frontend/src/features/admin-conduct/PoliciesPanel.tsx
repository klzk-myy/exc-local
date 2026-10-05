/**
 * Conduct policies panel (Phase-10.5 Task 10.5.3.15 §2) —
 * execution-policy versions (draft → CCO activate → annual review
 * with rolled deadline; activation supersedes the incumbent and is
 * frozen while review is overdue), product-profile create/update via
 * the dual-control queue (202 PENDING — never rendered as applied),
 * and the product target-market review queue (APPROVE|NARROW|SUSPEND).
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
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import {
  activateExecPolicy,
  draftExecPolicy,
  fetchExecPolicies,
  fetchProductProfiles,
  fetchTargetMarkets,
  reviewExecPolicy,
  reviewTargetMarket,
  submitProductProfile,
} from './api';

export function PoliciesPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [policy, setPolicy] = useState({ version: '', bodyRef: '' });
  const [profile, setProfile] = useState({
    profileId: '',
    reason: '',
    code: '',
    pricingPlan: 'FIXED',
    minDeposit: '0',
  });
  const [notice, setNotice] = useState<string | null>(null);

  const policies = useQuery({
    queryKey: ['admin-exec-policies'],
    queryFn: () => fetchExecPolicies(adminApi),
  });
  const profiles = useQuery({
    queryKey: ['admin-product-profiles'],
    queryFn: () => fetchProductProfiles(adminApi),
  });
  const tms = useQuery({
    queryKey: ['admin-target-markets'],
    queryFn: () => fetchTargetMarkets(adminApi, true),
  });
  const invalidate = () => {
    for (const k of ['admin-exec-policies', 'admin-product-profiles', 'admin-target-markets']) {
      void qc.invalidateQueries({ queryKey: [k] });
    }
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const draftMut = useMutation({
    mutationFn: () =>
      draftExecPolicy(adminApi, { version: policy.version, body_ref: policy.bodyRef }),
    onSuccess: () => {
      setNotice(`Execution policy v${policy.version} filed as DRAFT.`);
      invalidate();
    },
    onError: onErr,
  });
  const actMut = useMutation({
    mutationFn: (id: number) => activateExecPolicy(adminApi, id, {}),
    onSuccess: () => {
      setNotice(
        'Policy activation submitted — supersedes incumbent (frozen while review overdue).',
      );
      invalidate();
    },
    onError: onErr,
  });
  const reviewMut = useMutation({
    mutationFn: (id: number) => reviewExecPolicy(adminApi, id, {}),
    onSuccess: () => {
      setNotice('Annual review recorded — deadline rolled.');
      invalidate();
    },
    onError: onErr,
  });
  const profileMut = useMutation({
    mutationFn: () =>
      submitProductProfile(apiClient, adminApi.env, profile.profileId === '' ? 'POST' : 'PUT', {
        ...(profile.profileId !== '' ? { profile_id: Number(profile.profileId) } : {}),
        reason: profile.reason,
        input: {
          code: profile.code,
          pricing_plan: profile.pricingPlan,
          min_deposit: profile.minDeposit,
        },
      }),
    onSuccess: (r) => {
      setNotice(
        `Profile change pending dual-control approval (request #${r.requestId}) — not yet applied.`,
      );
      invalidate();
    },
    onError: onErr,
  });
  const tmReview = useMutation({
    mutationFn: (v: { id: number; action: 'APPROVE' | 'NARROW' | 'SUSPEND' }) =>
      reviewTargetMarket(adminApi, v.id, v.action),
    onSuccess: (_d, v) => {
      setNotice(`Target market #${v.id} reviewed: ${v.action}.`);
      invalidate();
    },
    onError: onErr,
  });

  const denied =
    (policies.error !== null && isAccessDenied(policies.error)) ||
    (profiles.error !== null && isAccessDenied(profiles.error)) ||
    (tms.error !== null && isAccessDenied(tms.error));
  if (denied) return <AccessDeniedCard />;

  return (
    <section className={cardCls} aria-label="Conduct policies">
      <h2 className="mb-2 text-sm font-semibold">Execution policies &amp; product governance</h2>

      <h3 className="mb-1 text-sm font-medium">Execution-policy versions</h3>
      {policies.error !== null ? <ErrorBox error={policies.error} /> : null}
      {policies.data !== undefined && policies.data.length > 0 ? (
        <div className="max-h-36 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Version</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Review due</th>
                <th className={thCls}>
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {policies.data.map((p) => (
                <tr key={p.id}>
                  <td className={tdCls}>{p.id}</td>
                  <td className={tdCls}>{p.version}</td>
                  <td className={tdCls}>
                    <StatusBadge value={p.status || 'DRAFT'} />
                  </td>
                  <td className={tdCls}>{p.reviewDueAt?.slice(0, 10) ?? '—'}</td>
                  <td className={tdCls}>
                    <div className="flex gap-1">
                      {p.status !== 'ACTIVE' ? (
                        <button
                          type="button"
                          className={btnGhost}
                          onClick={() => {
                            actMut.mutate(p.id);
                          }}
                        >
                          Activate
                        </button>
                      ) : null}
                      <button
                        type="button"
                        className={btnGhost}
                        onClick={() => {
                          reviewMut.mutate(p.id);
                        }}
                      >
                        Review
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      <form
        aria-label="Draft policy"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (policy.version !== '' && policy.bodyRef !== '') draftMut.mutate();
        }}
      >
        <input
          aria-label="Policy version"
          className={inputCls}
          placeholder="version"
          value={policy.version}
          onChange={(e) => {
            setPolicy({ ...policy, version: e.target.value });
          }}
        />
        <input
          aria-label="Policy body ref"
          className={inputCls}
          placeholder="body_ref"
          value={policy.bodyRef}
          onChange={(e) => {
            setPolicy({ ...policy, bodyRef: e.target.value });
          }}
        />
        <button type="submit" className={btnGhost} disabled={draftMut.isPending}>
          File draft
        </button>
      </form>

      <h3 className="mb-1 mt-4 text-sm font-medium">Product profiles (dual-control)</h3>
      {profiles.error !== null ? <ErrorBox error={profiles.error} /> : null}
      {profiles.data !== undefined && profiles.data.length > 0 ? (
        <div className="max-h-32 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Code</th>
                <th className={thCls}>Pricing</th>
                <th className={thCls}>Min deposit</th>
                <th className={thCls}>Status</th>
              </tr>
            </thead>
            <tbody>
              {profiles.data.map((p) => (
                <tr key={p.profileId}>
                  <td className={tdCls}>{p.profileId}</td>
                  <td className={tdCls}>{p.code}</td>
                  <td className={tdCls}>{p.pricingPlan}</td>
                  <td className={tdCls}>{p.minDeposit}</td>
                  <td className={tdCls}>
                    <StatusBadge value={p.status || 'ACTIVE'} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      <form
        aria-label="Submit profile change"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (profile.code !== '' && profile.reason !== '') profileMut.mutate();
        }}
      >
        <input
          aria-label="Profile id (blank = create)"
          className={inputCls}
          placeholder="profile_id (blank = create)"
          value={profile.profileId}
          onChange={(e) => {
            setProfile({ ...profile, profileId: e.target.value });
          }}
        />
        {(
          [
            ['code', 'code'],
            ['pricingPlan', 'pricing_plan'],
            ['minDeposit', 'min_deposit'],
            ['reason', 'reason'],
          ] as const
        ).map(([k, ph]) => (
          <input
            key={k}
            aria-label={`Profile ${k}`}
            className={inputCls}
            placeholder={ph}
            value={profile[k]}
            onChange={(e) => {
              setProfile({ ...profile, [k]: e.target.value });
            }}
          />
        ))}
        <button type="submit" className={btnPrimary} disabled={profileMut.isPending}>
          Submit for approval
        </button>
      </form>
      <p className={hintTextCls}>
        Profile changes are dual-control — a 202 means the request is QUEUED for a second Compliance
        Officer, never applied yet.
      </p>

      <h3 className="mb-1 mt-4 text-sm font-medium">Target-market review queue (overdue)</h3>
      {tms.error !== null ? <ErrorBox error={tms.error} /> : null}
      {tms.data?.length === 0 ? (
        <p className="text-sm text-neutral-500">No overdue reviews.</p>
      ) : null}
      {tms.data !== undefined && tms.data.length > 0 ? (
        <div className="max-h-32 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Profile</th>
                <th className={thCls}>Category</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Review due</th>
                <th className={thCls}>
                  <span className="sr-only">Review</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {tms.data.map((t) => (
                <tr key={t.id}>
                  <td className={tdCls}>{t.id}</td>
                  <td className={tdCls}>{t.profileId}</td>
                  <td className={tdCls}>{t.clientCategory}</td>
                  <td className={tdCls}>
                    <StatusBadge value={t.status || 'CURRENT'} />
                  </td>
                  <td className={tdCls}>{t.reviewDueAt.slice(0, 10)}</td>
                  <td className={tdCls}>
                    <div className="flex gap-1">
                      {(['APPROVE', 'NARROW', 'SUSPEND'] as const).map((a) => (
                        <button
                          key={a}
                          type="button"
                          className={btnGhost}
                          onClick={() => {
                            tmReview.mutate({ id: t.id, action: a });
                          }}
                        >
                          {a}
                        </button>
                      ))}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
