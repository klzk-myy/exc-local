/**
 * Venue member admission & lifecycle panel (Phase-10.5 Task
 * 10.5.3.14 §1) — the member/DEA/sponsored register with the full
 * admission pipeline: application → due-diligence → agreements →
 * approved products/ports → admission decision (server-gated on DD
 * COMPLETED + ≥1 agreement) → suspend/reinstate/terminate → appeals
 * and periodic reviews. Every mutation lands on the member's
 * immutable lifecycle ledger (rendered as the event log).
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
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import { fetchMember, fetchMembers, memberAction, registerMember } from './api';

const ACTIONS = [
  'due-diligence',
  'agreements',
  'products',
  'decision',
  'suspend',
  'reinstate',
  'terminate',
  'appeals',
  'appeal-decision',
  'reviews',
] as const;
type Action = (typeof ACTIONS)[number];

export function MembersPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [selectedId, setSelectedId] = useState<number | null>(null);
  const [action, setAction] = useState<Action>('due-diligence');
  const [fields, setFields] = useState<Record<string, string>>({});
  const [app, setApp] = useState({
    legalName: '',
    lei: '',
    accessModel: 'MEMBER',
    regulatoryStatus: '',
    jurisdiction: '',
  });
  const [notice, setNotice] = useState<string | null>(null);

  const members = useQuery({
    queryKey: ['admin-venue-members'],
    queryFn: () => fetchMembers(adminApi),
  });
  const detail = useQuery({
    queryKey: ['admin-venue-member', selectedId],
    queryFn: () => fetchMember(adminApi, selectedId ?? 0),
    enabled: selectedId !== null,
  });
  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-venue-members'] });
    void qc.invalidateQueries({ queryKey: ['admin-venue-member'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const register = useMutation({
    mutationFn: () =>
      registerMember(adminApi, {
        legal_name: app.legalName,
        lei: app.lei,
        access_model: app.accessModel,
        ...(app.regulatoryStatus !== '' ? { regulatory_status: app.regulatoryStatus } : {}),
        ...(app.jurisdiction !== '' ? { jurisdiction: app.jurisdiction } : {}),
      }),
    onSuccess: (id) => {
      setNotice(`Member application #${id} registered (idempotent on LEI).`);
      invalidate();
    },
    onError: onErr,
  });

  const act = useMutation({
    mutationFn: () => {
      if (selectedId === null) return Promise.reject(new Error('Select a member'));
      const f = fields;
      const body: Record<string, unknown> = (() => {
        switch (action) {
          case 'due-diligence':
            return { status: f.status ?? 'COMPLETED' };
          case 'agreements':
            return { kind: f.kind ?? '', ref: f.ref ?? '' };
          case 'products':
            return {
              products: (f.products ?? '')
                .split(',')
                .map((s) => s.trim())
                .filter((s) => s !== ''),
              ports: (f.ports ?? '')
                .split(',')
                .map((s) => s.trim())
                .filter((s) => s !== ''),
            };
          case 'decision':
            return {
              approve: (f.approve ?? 'true') === 'true',
              reason: f.reason ?? '',
              annual_review_due: f.annual_review_due ?? '',
            };
          case 'suspend':
          case 'reinstate':
          case 'terminate':
            return { reason: f.reason ?? '' };
          case 'appeals':
            return { grounds: f.grounds ?? '' };
          case 'appeal-decision':
            return { uphold: (f.uphold ?? 'true') === 'true', rationale: f.rationale ?? '' };
          case 'reviews':
            return {
              review_type: f.review_type ?? 'ANNUAL',
              outcome: f.outcome ?? 'PASS',
              next_review_due: f.next_review_due ?? '',
            };
        }
      })();
      return memberAction(adminApi, selectedId, action, body);
    },
    onSuccess: () => {
      setNotice(`Member #${selectedId} ${action} recorded on the lifecycle ledger.`);
      invalidate();
    },
    onError: onErr,
  });

  if (members.error !== null && isAccessDenied(members.error)) {
    return <AccessDeniedCard />;
  }

  const input = (key: string, ph: string) => (
    <input
      aria-label={`Member action ${key}`}
      className={inputCls}
      placeholder={ph}
      value={fields[key] ?? ''}
      onChange={(e) => {
        setFields({ ...fields, [key]: e.target.value });
      }}
    />
  );

  return (
    <section className={cardCls} aria-label="Venue members">
      <h2 className="mb-2 text-sm font-semibold">Member admission &amp; lifecycle</h2>
      {members.error !== null ? <ErrorBox error={members.error} /> : null}
      {members.data?.length === 0 ? (
        <p className="text-sm text-neutral-500">No members registered.</p>
      ) : null}
      {members.data !== undefined && members.data.length > 0 ? (
        <div className="max-h-48 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Legal name</th>
                <th className={thCls}>LEI</th>
                <th className={thCls}>Model</th>
                <th className={thCls}>DD</th>
                <th className={thCls}>Admission</th>
                <th className={thCls}>Flags</th>
              </tr>
            </thead>
            <tbody>
              {members.data.map((m) => (
                <tr
                  key={m.memberId}
                  className="cursor-pointer hover:bg-neutral-800/50"
                  onClick={() => {
                    setSelectedId(m.memberId);
                  }}
                >
                  <td className={tdCls}>{m.memberId}</td>
                  <td className={tdCls}>{m.legalName}</td>
                  <td className={`${tdCls} font-mono text-xs`}>{m.lei}</td>
                  <td className={tdCls}>{m.accessModel}</td>
                  <td className={tdCls}>
                    <StatusBadge value={m.dueDiligenceStatus || 'PENDING'} />
                  </td>
                  <td className={tdCls}>
                    <StatusBadge value={m.admissionDecision || 'PENDING'} />
                  </td>
                  <td className={tdCls}>
                    {m.suspended ? 'SUSPENDED ' : ''}
                    {m.terminatedAt !== undefined ? 'TERMINATED' : ''}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      <form
        aria-label="Register member"
        className="mt-3 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (app.legalName !== '' && app.lei !== '') register.mutate();
        }}
      >
        {(
          [
            ['legalName', 'legal_name'],
            ['lei', 'LEI (checksum-validated)'],
            ['regulatoryStatus', 'regulatory_status'],
            ['jurisdiction', 'jurisdiction'],
          ] as const
        ).map(([k, ph]) => (
          <input
            key={k}
            aria-label={`App ${k}`}
            className={inputCls}
            placeholder={ph}
            value={app[k]}
            onChange={(e) => {
              setApp({ ...app, [k]: e.target.value });
            }}
          />
        ))}
        <select
          aria-label="Access model"
          className={selectCls}
          value={app.accessModel}
          onChange={(e) => {
            setApp({ ...app, accessModel: e.target.value });
          }}
        >
          <option value="MEMBER">MEMBER</option>
          <option value="DEA">DEA</option>
          <option value="SPONSORED">SPONSORED</option>
        </select>
        <button type="submit" className={btnPrimary} disabled={register.isPending}>
          Register application
        </button>
      </form>

      {detail.data !== undefined ? (
        <div className="mt-3 rounded border border-neutral-800 p-2">
          <p className="mb-1 text-sm font-medium">
            Member #{detail.data.member.memberId} — {detail.data.member.legalName}
            {detail.data.member.annualReviewDue !== undefined
              ? ` · next review ${detail.data.member.annualReviewDue.slice(0, 10)}`
              : ''}
          </p>
          {detail.data.member.approvedProducts.length > 0 ? (
            <p className={hintTextCls}>
              Products: {detail.data.member.approvedProducts.join(', ')}
            </p>
          ) : null}
          {detail.data.events.length > 0 ? (
            <div className="mt-1 max-h-28 overflow-y-auto">
              {detail.data.events.map((ev) => (
                <p key={ev.eventId} className="font-mono text-xs text-neutral-400">
                  {ev.createdAt.slice(0, 16)} · {ev.eventType}
                </p>
              ))}
            </div>
          ) : null}
          {detail.data.reviews.length > 0 ? (
            <div className="mt-1">
              {detail.data.reviews.map((rv) => (
                <p key={rv.reviewId} className="text-xs text-neutral-400">
                  {rv.reviewedAt.slice(0, 10)} · {rv.reviewType} · {rv.outcome} · next{' '}
                  {rv.nextReviewDue.slice(0, 10)}
                </p>
              ))}
            </div>
          ) : null}

          <form
            aria-label="Member action"
            className="mt-2 flex flex-wrap items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              act.mutate();
            }}
          >
            <select
              aria-label="Lifecycle action"
              className={selectCls}
              value={action}
              onChange={(e) => {
                setAction(e.target.value as Action);
                setFields({});
              }}
            >
              {ACTIONS.map((a) => (
                <option key={a} value={a}>
                  {a}
                </option>
              ))}
            </select>
            {action === 'due-diligence' ? (
              <select
                aria-label="DD status"
                className={selectCls}
                value={fields.status ?? 'COMPLETED'}
                onChange={(e) => {
                  setFields({ ...fields, status: e.target.value });
                }}
              >
                {['PENDING', 'IN_PROGRESS', 'COMPLETED', 'REJECTED'].map((s) => (
                  <option key={s} value={s}>
                    {s}
                  </option>
                ))}
              </select>
            ) : null}
            {action === 'agreements' ? (
              <>
                {input('kind', 'kind (e.g. RULEBOOK)')}
                {input('ref', 'executed ref')}
              </>
            ) : null}
            {action === 'products' ? (
              <>
                {input('products', 'products (csv)')}
                {input('ports', 'ports (csv)')}
              </>
            ) : null}
            {action === 'decision' ? (
              <>
                <select
                  aria-label="Admission approve"
                  className={selectCls}
                  value={fields.approve ?? 'true'}
                  onChange={(e) => {
                    setFields({ ...fields, approve: e.target.value });
                  }}
                >
                  <option value="true">APPROVE</option>
                  <option value="false">REJECT</option>
                </select>
                {input('reason', 'reason')}
                {input('annual_review_due', 'annual_review_due')}
              </>
            ) : null}
            {action === 'suspend' || action === 'reinstate' || action === 'terminate'
              ? input('reason', 'reason')
              : null}
            {action === 'appeals' ? input('grounds', 'appeal grounds') : null}
            {action === 'appeal-decision' ? (
              <>
                <select
                  aria-label="Appeal uphold"
                  className={selectCls}
                  value={fields.uphold ?? 'true'}
                  onChange={(e) => {
                    setFields({ ...fields, uphold: e.target.value });
                  }}
                >
                  <option value="true">UPHOLD</option>
                  <option value="false">DENY</option>
                </select>
                {input('rationale', 'rationale')}
              </>
            ) : null}
            {action === 'reviews' ? (
              <>
                <select
                  aria-label="Review outcome"
                  className={selectCls}
                  value={fields.outcome ?? 'PASS'}
                  onChange={(e) => {
                    setFields({ ...fields, outcome: e.target.value });
                  }}
                >
                  {['PASS', 'CONDITIONAL', 'FAIL'].map((o) => (
                    <option key={o} value={o}>
                      {o}
                    </option>
                  ))}
                </select>
                {input('next_review_due', 'next_review_due')}
              </>
            ) : null}
            <button type="submit" className={btnGhost} disabled={act.isPending}>
              Apply
            </button>
          </form>
          <p className={hintTextCls}>
            Admission APPROVE is server-gated on DD COMPLETED + ≥1 executed agreement; FAIL review
            auto-suspends; terminate retains the record ≥5y.
          </p>
        </div>
      ) : null}
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
