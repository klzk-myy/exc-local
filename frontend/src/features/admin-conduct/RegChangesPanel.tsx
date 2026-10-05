/**
 * Regulatory-change management panel (Phase-10.5 Task 10.5.3.15 §1)
 * — the regulatory watch register with the 10-business-day triage SLA
 * clock, impact assessment records, lifecycle transitions
 * (triage|scope|implement|close|assign_owner — IMPLEMENTED is gated
 * server-side on a completed assessment), regulator correspondence
 * (INFO_HOLD|INFO_REQUEST), and per-impact-item completion.
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
  attachCorrespondence,
  completeImpactItem,
  createRegChange,
  fetchRegChangeImpact,
  fetchRegChanges,
  putRegChangeImpact,
  transitionRegChange,
} from './api';

export function RegChangesPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [selectedId, setSelectedId] = useState<number | null>(null);
  const [form, setForm] = useState({
    authority: '',
    instrument: '',
    title: '',
    publishedAt: '',
    effectiveAt: '',
    sourceUrl: '',
  });
  const [impact, setImpact] = useState({ kind: '', ref: '', effort: '', dueAt: '' });
  const [transition, setTransition] = useState({ action: 'triage', owner: '', notes: '' });
  const [corr, setCorr] = useState({
    kind: 'INFO_REQUEST',
    summary: '',
    receivedAt: '',
    dueAt: '',
  });
  const [notice, setNotice] = useState<string | null>(null);

  const changes = useQuery({
    queryKey: ['admin-reg-changes'],
    queryFn: () => fetchRegChanges(adminApi),
  });
  const detail = useQuery({
    queryKey: ['admin-reg-change', selectedId],
    queryFn: () => fetchRegChangeImpact(adminApi, selectedId ?? 0),
    enabled: selectedId !== null,
  });
  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-reg-changes'] });
    void qc.invalidateQueries({ queryKey: ['admin-reg-change'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const create = useMutation({
    mutationFn: () =>
      createRegChange(adminApi, {
        authority: form.authority,
        instrument: form.instrument,
        title: form.title,
        published_at: form.publishedAt,
        ...(form.effectiveAt !== '' ? { effective_at: form.effectiveAt } : {}),
        ...(form.sourceUrl !== '' ? { source_url: form.sourceUrl } : {}),
      }),
    onSuccess: (r) => {
      setNotice(
        r.created
          ? `Change #${r.id} registered${r.overlapping.length > 0 ? ` — overlaps: ${r.overlapping.join(',')}` : ''}.`
          : `Change #${r.id} already registered (dedup).`,
      );
      invalidate();
    },
    onError: onErr,
  });
  const impactMut = useMutation({
    mutationFn: () => {
      if (selectedId === null) return Promise.reject(new Error('Select a change'));
      return putRegChangeImpact(apiClient, adminApi.env, selectedId, {
        kind: impact.kind,
        ref: impact.ref,
        ...(impact.effort !== '' ? { effort_estimate: impact.effort } : {}),
        ...(impact.dueAt !== '' ? { due_at: impact.dueAt } : {}),
      });
    },
    onSuccess: () => {
      setNotice('Impact assessment recorded.');
      invalidate();
    },
    onError: onErr,
  });
  const transMut = useMutation({
    mutationFn: () => {
      if (selectedId === null) return Promise.reject(new Error('Select a change'));
      return transitionRegChange(adminApi, selectedId, {
        action: transition.action,
        ...(transition.owner !== '' ? { owner: Number(transition.owner) } : {}),
        ...(transition.notes !== '' ? { notes: transition.notes } : {}),
      });
    },
    onSuccess: () => {
      setNotice(`Change #${selectedId} — ${transition.action} applied.`);
      invalidate();
    },
    onError: onErr,
  });
  const corrMut = useMutation({
    mutationFn: () => {
      if (selectedId === null) return Promise.reject(new Error('Select a change'));
      return attachCorrespondence(adminApi, selectedId, {
        kind: corr.kind,
        summary: corr.summary,
        received_at: corr.receivedAt,
        ...(corr.dueAt !== '' ? { due_at: corr.dueAt } : {}),
      });
    },
    onSuccess: () => {
      setNotice('Correspondence attached.');
      invalidate();
    },
    onError: onErr,
  });
  const impactDone = useMutation({
    mutationFn: (impactId: number) => completeImpactItem(adminApi, impactId),
    onSuccess: () => {
      setNotice('Impact item marked done.');
      invalidate();
    },
    onError: onErr,
  });

  if (changes.error !== null && isAccessDenied(changes.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Reg changes">
      <h2 className="mb-2 text-sm font-semibold">Regulatory changes</h2>
      {changes.error !== null ? <ErrorBox error={changes.error} /> : null}
      {changes.data?.length === 0 ? (
        <p className="text-sm text-neutral-500">No registered changes.</p>
      ) : null}
      {changes.data !== undefined && changes.data.length > 0 ? (
        <div className="max-h-44 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Authority</th>
                <th className={thCls}>Instrument</th>
                <th className={thCls}>Title</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Triage SLA</th>
              </tr>
            </thead>
            <tbody>
              {changes.data.map((c) => (
                <tr
                  key={c.changeId}
                  className="cursor-pointer hover:bg-neutral-800/50"
                  onClick={() => {
                    setSelectedId(c.changeId);
                  }}
                >
                  <td className={tdCls}>{c.changeId}</td>
                  <td className={tdCls}>{c.authority}</td>
                  <td className={tdCls}>{c.instrument}</td>
                  <td className={tdCls}>{c.title}</td>
                  <td className={tdCls}>
                    <StatusBadge value={c.status || 'RECEIVED'} />
                  </td>
                  <td className={tdCls}>
                    {c.triagedAt !== undefined
                      ? `triaged ${c.triagedAt.slice(0, 10)}`
                      : `due ${c.triageDueAt.slice(0, 10)}`}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      <form
        aria-label="Register change"
        className="mt-3 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (form.authority !== '' && form.title !== '' && form.publishedAt !== '') {
            create.mutate();
          }
        }}
      >
        {(
          [
            ['authority', 'authority (FCA/ESMA/…)'],
            ['instrument', 'instrument'],
            ['title', 'title'],
            ['publishedAt', 'published_at'],
            ['effectiveAt', 'effective_at'],
            ['sourceUrl', 'source_url'],
          ] as const
        ).map(([k, ph]) => (
          <input
            key={k}
            aria-label={`Change ${k}`}
            className={inputCls}
            placeholder={ph}
            value={form[k]}
            onChange={(e) => {
              setForm({ ...form, [k]: e.target.value });
            }}
          />
        ))}
        <button type="submit" className={btnPrimary} disabled={create.isPending}>
          Register
        </button>
      </form>

      {detail.data !== undefined ? (
        <div className="mt-3 rounded border border-neutral-800 p-2">
          <p className="mb-1 text-sm font-medium">
            #{detail.data.change.changeId} {detail.data.change.title} —{' '}
            <StatusBadge value={detail.data.change.status} />
          </p>
          {detail.data.impacts.length > 0 ? (
            <div className="mt-1">
              {detail.data.impacts.map((i) => (
                <div key={i.id} className="flex items-center gap-2 text-xs text-neutral-400">
                  <span>
                    #{i.id} · {i.kind} · {i.ref} · {i.status}
                    {i.dueAt !== undefined ? ` · due ${i.dueAt.slice(0, 10)}` : ''}
                  </span>
                  {i.completedAt === undefined ? (
                    <button
                      type="button"
                      className={btnGhost}
                      onClick={() => {
                        impactDone.mutate(i.id);
                      }}
                    >
                      Done
                    </button>
                  ) : null}
                </div>
              ))}
            </div>
          ) : null}
          {detail.data.correspondence.length > 0 ? (
            <div className="mt-1">
              {detail.data.correspondence.map((c) => (
                <p key={c.id} className="text-xs text-neutral-400">
                  {c.kind} · {c.summary} · recv {c.receivedAt.slice(0, 10)}
                  {c.dueAt !== undefined ? ` · due ${c.dueAt.slice(0, 10)}` : ''}
                </p>
              ))}
            </div>
          ) : null}

          <form
            aria-label="Impact assessment"
            className="mt-2 flex flex-wrap items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              if (impact.kind !== '' && impact.ref !== '') impactMut.mutate();
            }}
          >
            {(
              [
                ['kind', 'kind'],
                ['ref', 'ref (doc/ticket)'],
                ['effort', 'effort_estimate'],
                ['dueAt', 'due_at'],
              ] as const
            ).map(([k, ph]) => (
              <input
                key={k}
                aria-label={`Impact ${k}`}
                className={inputCls}
                placeholder={ph}
                value={impact[k]}
                onChange={(e) => {
                  setImpact({ ...impact, [k]: e.target.value });
                }}
              />
            ))}
            <button type="submit" className={btnGhost} disabled={impactMut.isPending}>
              Record impact
            </button>
          </form>
          <form
            aria-label="Change transition"
            className="mt-2 flex flex-wrap items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              transMut.mutate();
            }}
          >
            <select
              aria-label="Transition action"
              className={selectCls}
              value={transition.action}
              onChange={(e) => {
                setTransition({ ...transition, action: e.target.value });
              }}
            >
              {['triage', 'scope', 'implement', 'close', 'assign_owner'].map((a) => (
                <option key={a} value={a}>
                  {a}
                </option>
              ))}
            </select>
            <input
              aria-label="Transition owner"
              className={inputCls}
              placeholder="owner user_id"
              value={transition.owner}
              onChange={(e) => {
                setTransition({ ...transition, owner: e.target.value });
              }}
            />
            <input
              aria-label="Transition notes"
              className={inputCls}
              placeholder="notes"
              value={transition.notes}
              onChange={(e) => {
                setTransition({ ...transition, notes: e.target.value });
              }}
            />
            <button type="submit" className={btnGhost} disabled={transMut.isPending}>
              Transition
            </button>
          </form>
          <form
            aria-label="Attach correspondence"
            className="mt-2 flex flex-wrap items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              if (corr.summary !== '' && corr.receivedAt !== '') corrMut.mutate();
            }}
          >
            <select
              aria-label="Correspondence kind"
              className={selectCls}
              value={corr.kind}
              onChange={(e) => {
                setCorr({ ...corr, kind: e.target.value });
              }}
            >
              <option value="INFO_REQUEST">INFO_REQUEST</option>
              <option value="INFO_HOLD">INFO_HOLD</option>
            </select>
            {(
              [
                ['summary', 'summary'],
                ['receivedAt', 'received_at'],
                ['dueAt', 'due_at'],
              ] as const
            ).map(([k, ph]) => (
              <input
                key={k}
                aria-label={`Corr ${k}`}
                className={inputCls}
                placeholder={ph}
                value={corr[k]}
                onChange={(e) => {
                  setCorr({ ...corr, [k]: e.target.value });
                }}
              />
            ))}
            <button type="submit" className={btnGhost} disabled={corrMut.isPending}>
              Attach
            </button>
          </form>
          <p className={hintTextCls}>
            IMPLEMENTED is refused until a completed impact assessment exists — server-side gate;
            triage SLA is 10 business days from receipt.
          </p>
        </div>
      ) : null}
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
