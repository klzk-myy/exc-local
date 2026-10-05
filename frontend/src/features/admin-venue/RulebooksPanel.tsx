/**
 * Rulebook governance panel (Phase-10.5 Task 10.5.3.14 §2) —
 * rulebook/product-terms versions through the DRAFT → FILED →
 * APPROVED → ACTIVE lifecycle: regulator filing + verdict ingestion,
 * venue/CCO approval, activation (emergency activations carry a
 * reason), participant notices, and member acknowledgement evidence.
 * The server refuses activation before required approvals/notice
 * (VENUE_RULEBOOK_NOT_APPROVED) — the UI surfaces that error verbatim.
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
import { draftRulebook, fetchRulebook, fetchRulebooks, rulebookAction } from './api';

const ACTIONS = ['file', 'regulator-decision', 'approve', 'activate', 'notices', 'acks'] as const;
type Action = (typeof ACTIONS)[number];

export function RulebooksPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [selectedId, setSelectedId] = useState<number | null>(null);
  const [action, setAction] = useState<Action>('approve');
  const [fields, setFields] = useState<Record<string, string>>({});
  const [draft, setDraft] = useState({
    kind: 'RULEBOOK',
    scopeKey: '',
    version: '',
    bodyRef: '',
    requiresRegulator: false,
  });
  const [notice, setNotice] = useState<string | null>(null);

  const rulebooks = useQuery({
    queryKey: ['admin-venue-rulebooks'],
    queryFn: () => fetchRulebooks(adminApi),
  });
  const detail = useQuery({
    queryKey: ['admin-venue-rulebook', selectedId],
    queryFn: () => fetchRulebook(adminApi, selectedId ?? 0),
    enabled: selectedId !== null,
  });
  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-venue-rulebooks'] });
    void qc.invalidateQueries({ queryKey: ['admin-venue-rulebook'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const draftMut = useMutation({
    mutationFn: () =>
      draftRulebook(adminApi, {
        kind: draft.kind,
        ...(draft.scopeKey !== '' ? { scope_key: draft.scopeKey } : {}),
        version: draft.version,
        body_ref: draft.bodyRef,
        requires_regulator_approval: draft.requiresRegulator,
      }),
    onSuccess: (id) => {
      setNotice(`Rulebook draft #${id} filed (idempotent on kind/scope/version).`);
      invalidate();
    },
    onError: onErr,
  });

  const act = useMutation({
    mutationFn: () => {
      if (selectedId === null) return Promise.reject(new Error('Select a rulebook'));
      const f = fields;
      const body: Record<string, unknown> = (() => {
        switch (action) {
          case 'file':
            return { filing_ref: f.filing_ref ?? '' };
          case 'regulator-decision':
            return { approved: (f.approved ?? 'true') === 'true', notes: f.notes ?? '' };
          case 'approve':
            return {};
          case 'activate':
            return {
              ...(f.effective_from !== undefined && f.effective_from !== ''
                ? { effective_from: f.effective_from }
                : {}),
              emergency: (f.emergency ?? 'false') === 'true',
              ...(f.emergency_reason !== undefined && f.emergency_reason !== ''
                ? { emergency_reason: f.emergency_reason }
                : {}),
            };
          case 'notices':
            return {
              subject: f.subject ?? '',
              body_ref: f.body_ref ?? '',
              ...(f.member_id !== undefined && f.member_id !== ''
                ? { member_id: Number(f.member_id) }
                : {}),
            };
          case 'acks':
            return {
              member_id: Number(f.member_id ?? '0'),
              ...(f.notice_id !== undefined && f.notice_id !== ''
                ? { notice_id: Number(f.notice_id) }
                : {}),
              acknowledged_by: f.acknowledged_by ?? '',
            };
        }
      })();
      return rulebookAction(adminApi, selectedId, action, body);
    },
    onSuccess: () => {
      setNotice(`Rulebook #${selectedId} ${action} applied.`);
      invalidate();
    },
    onError: onErr,
  });

  if (rulebooks.error !== null && isAccessDenied(rulebooks.error)) {
    return <AccessDeniedCard />;
  }

  const input = (key: string, ph: string) => (
    <input
      aria-label={`Rulebook action ${key}`}
      className={inputCls}
      placeholder={ph}
      value={fields[key] ?? ''}
      onChange={(e) => {
        setFields({ ...fields, [key]: e.target.value });
      }}
    />
  );

  return (
    <section className={cardCls} aria-label="Venue rulebooks">
      <h2 className="mb-2 text-sm font-semibold">Rulebook &amp; product-terms versions</h2>
      {rulebooks.error !== null ? <ErrorBox error={rulebooks.error} /> : null}
      {rulebooks.data?.length === 0 ? (
        <p className="text-sm text-neutral-500">No rulebook versions.</p>
      ) : null}
      {rulebooks.data !== undefined && rulebooks.data.length > 0 ? (
        <div className="max-h-44 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Kind</th>
                <th className={thCls}>Scope</th>
                <th className={thCls}>Version</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Regulator</th>
                <th className={thCls}>Effective</th>
              </tr>
            </thead>
            <tbody>
              {rulebooks.data.map((rb) => (
                <tr
                  key={rb.rulebookId}
                  className="cursor-pointer hover:bg-neutral-800/50"
                  onClick={() => {
                    setSelectedId(rb.rulebookId);
                  }}
                >
                  <td className={tdCls}>{rb.rulebookId}</td>
                  <td className={tdCls}>{rb.kind}</td>
                  <td className={tdCls}>{rb.scopeKey}</td>
                  <td className={tdCls}>{rb.version}</td>
                  <td className={tdCls}>
                    <StatusBadge value={rb.status || 'DRAFT'} />
                  </td>
                  <td className={tdCls}>
                    {rb.requiresRegulator ? rb.regulatorStatus || 'REQUIRED' : 'n/a'}
                  </td>
                  <td className={tdCls}>{rb.effectiveFrom?.slice(0, 10) ?? '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      <form
        aria-label="Draft rulebook"
        className="mt-3 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (draft.version !== '' && draft.bodyRef !== '') draftMut.mutate();
        }}
      >
        <select
          aria-label="Draft kind"
          className={selectCls}
          value={draft.kind}
          onChange={(e) => {
            setDraft({ ...draft, kind: e.target.value });
          }}
        >
          <option value="RULEBOOK">RULEBOOK</option>
          <option value="PRODUCT_TERMS">PRODUCT_TERMS</option>
        </select>
        {(
          [
            ['scopeKey', 'scope_key'],
            ['version', 'version'],
            ['bodyRef', 'body_ref (doc ref)'],
          ] as const
        ).map(([k, ph]) => (
          <input
            key={k}
            aria-label={`Draft ${k}`}
            className={inputCls}
            placeholder={ph}
            value={draft[k]}
            onChange={(e) => {
              setDraft({ ...draft, [k]: e.target.value });
            }}
          />
        ))}
        <label className="flex items-center gap-1 text-sm">
          <input
            type="checkbox"
            checked={draft.requiresRegulator}
            onChange={(e) => {
              setDraft({ ...draft, requiresRegulator: e.target.checked });
            }}
          />
          regulator approval
        </label>
        <button type="submit" className={btnPrimary} disabled={draftMut.isPending}>
          File draft
        </button>
      </form>

      {detail.data !== undefined ? (
        <div className="mt-3 rounded border border-neutral-800 p-2">
          <p className="mb-1 text-sm font-medium">
            Rulebook #{detail.data.rulebook.rulebookId} {detail.data.rulebook.version} —{' '}
            <StatusBadge value={detail.data.rulebook.status || 'DRAFT'} />
            {detail.data.rulebook.emergency ? ' · EMERGENCY' : ''}
          </p>
          {detail.data.notices.length > 0 ? (
            <div className="mt-1">
              {detail.data.notices.map((n) => (
                <p key={n.noticeId} className="text-xs text-neutral-400">
                  #{n.noticeId} · {n.issuedAt.slice(0, 10)} · {n.subject}
                  {n.memberId !== undefined ? ` · member ${n.memberId}` : ' · broadcast'}
                </p>
              ))}
            </div>
          ) : null}
          {detail.data.acks.length > 0 ? (
            <div className="mt-1">
              {detail.data.acks.map((a) => (
                <p key={a.ackId} className="text-xs text-neutral-500">
                  ack #{a.ackId} · member {a.memberId} · {a.acknowledgedBy} ·{' '}
                  {a.acknowledgedAt.slice(0, 10)}
                </p>
              ))}
            </div>
          ) : null}

          <form
            aria-label="Rulebook action"
            className="mt-2 flex flex-wrap items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              act.mutate();
            }}
          >
            <select
              aria-label="Rulebook lifecycle action"
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
            {action === 'file' ? input('filing_ref', 'regulator filing_ref') : null}
            {action === 'regulator-decision' ? (
              <>
                <select
                  aria-label="Regulator approved"
                  className={selectCls}
                  value={fields.approved ?? 'true'}
                  onChange={(e) => {
                    setFields({ ...fields, approved: e.target.value });
                  }}
                >
                  <option value="true">APPROVED</option>
                  <option value="false">REJECTED</option>
                </select>
                {input('notes', 'notes')}
              </>
            ) : null}
            {action === 'activate' ? (
              <>
                {input('effective_from', 'effective_from')}
                <select
                  aria-label="Emergency activation"
                  className={selectCls}
                  value={fields.emergency ?? 'false'}
                  onChange={(e) => {
                    setFields({ ...fields, emergency: e.target.value });
                  }}
                >
                  <option value="false">normal</option>
                  <option value="true">EMERGENCY</option>
                </select>
                {fields.emergency === 'true' ? input('emergency_reason', 'emergency_reason') : null}
              </>
            ) : null}
            {action === 'notices' ? (
              <>
                {input('subject', 'subject')}
                {input('body_ref', 'body_ref')}
                {input('member_id', 'member_id (blank = broadcast)')}
              </>
            ) : null}
            {action === 'acks' ? (
              <>
                {input('member_id', 'member_id')}
                {input('notice_id', 'notice_id')}
                {input('acknowledged_by', 'acknowledged_by')}
              </>
            ) : null}
            <button type="submit" className={btnGhost} disabled={act.isPending}>
              Apply
            </button>
          </form>
          <p className={hintTextCls}>
            Activation is refused (VENUE_RULEBOOK_NOT_APPROVED) until required regulator approvals
            and participant-notice obligations are met.
          </p>
        </div>
      ) : null}
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
