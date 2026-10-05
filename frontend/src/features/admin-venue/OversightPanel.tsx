/**
 * Venue oversight panel (Phase-10.5 Task 10.5.3.14 §2 continued) —
 * market-control interventions (record/lift, records retained),
 * investigation & disciplinary cases with append-only evidence and
 * the legal transition table (OPEN → INVESTIGATING → CHARGED →
 * SANCTIONED/DISMISSED → CLOSED; terminal moves require an outcome),
 * and the conflicts-of-interest register (declare/resolve).
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnGhost,
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
  attachCaseEvidence,
  declareConflict,
  fetchConflicts,
  fetchInterventions,
  fetchVenueCase,
  fetchVenueCases,
  liftIntervention,
  openVenueCase,
  recordIntervention,
  resolveConflict,
  transitionCase,
} from './api';

/** Legal next transitions per venue.Service.TransitionCase. */
const CASE_TRANSITIONS: Record<string, string[]> = {
  OPEN: ['INVESTIGATING', 'DISMISSED'],
  INVESTIGATING: ['CHARGED', 'DISMISSED', 'CLOSED'],
  CHARGED: ['SANCTIONED', 'DISMISSED'],
  SANCTIONED: ['CLOSED'],
  DISMISSED: ['CLOSED'],
};
const TERMINAL = new Set(['SANCTIONED', 'DISMISSED', 'CLOSED']);

const INTERVENTION_KINDS = [
  'LIMIT',
  'HALT',
  'CANCELLATION',
  'CORRECTION',
  'INFO_REQUEST',
  'POSITION_ACCOUNTABILITY',
  'EMERGENCY_RULE',
] as const;

export function OversightPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [selectedCase, setSelectedCase] = useState<number | null>(null);
  const [caseAction, setCaseAction] = useState<'evidence' | 'transition'>('evidence');
  const [fields, setFields] = useState<Record<string, string>>({});
  const [iv, setIv] = useState({ kind: 'HALT', reason: '', memberId: '', accountId: '' });
  const [caseForm, setCaseForm] = useState({
    kind: 'INVESTIGATION',
    subject: '',
    memberId: '',
    accountId: '',
  });
  const [coi, setCoi] = useState({ memberId: '', officerId: '', subject: '', nature: '' });
  const [coiResolve, setCoiResolve] = useState({ id: '', status: 'MITIGATED', mitigation: '' });
  const [notice, setNotice] = useState<string | null>(null);

  const interventions = useQuery({
    queryKey: ['admin-venue-interventions'],
    queryFn: () => fetchInterventions(adminApi),
  });
  const cases = useQuery({
    queryKey: ['admin-venue-cases'],
    queryFn: () => fetchVenueCases(adminApi),
  });
  const caseDetail = useQuery({
    queryKey: ['admin-venue-case', selectedCase],
    queryFn: () => fetchVenueCase(adminApi, selectedCase ?? 0),
    enabled: selectedCase !== null,
  });
  const conflicts = useQuery({
    queryKey: ['admin-venue-conflicts'],
    queryFn: () => fetchConflicts(adminApi),
  });
  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-venue-interventions'] });
    void qc.invalidateQueries({ queryKey: ['admin-venue-cases'] });
    void qc.invalidateQueries({ queryKey: ['admin-venue-case'] });
    void qc.invalidateQueries({ queryKey: ['admin-venue-conflicts'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const ivMut = useMutation({
    mutationFn: () =>
      recordIntervention(adminApi, {
        kind: iv.kind,
        reason: iv.reason,
        ...(iv.memberId !== '' ? { member_id: Number(iv.memberId) } : {}),
        ...(iv.accountId !== '' ? { account_id: Number(iv.accountId) } : {}),
      }),
    onSuccess: () => {
      setNotice('Intervention recorded (immutable evidence).');
      invalidate();
    },
    onError: onErr,
  });
  const lift = useMutation({
    mutationFn: (id: number) => liftIntervention(adminApi, id),
    onSuccess: () => {
      setNotice('Intervention lifted — record retained.');
      invalidate();
    },
    onError: onErr,
  });
  const openCase = useMutation({
    mutationFn: () =>
      openVenueCase(adminApi, {
        kind: caseForm.kind,
        subject: caseForm.subject,
        ...(caseForm.memberId !== '' ? { member_id: Number(caseForm.memberId) } : {}),
        ...(caseForm.accountId !== '' ? { account_id: Number(caseForm.accountId) } : {}),
      }),
    onSuccess: (id) => {
      setNotice(`Case #${id} opened.`);
      invalidate();
    },
    onError: onErr,
  });
  const caseAct = useMutation({
    mutationFn: () => {
      if (selectedCase === null) return Promise.reject(new Error('Select a case'));
      if (caseAction === 'evidence') {
        return attachCaseEvidence(adminApi, selectedCase, {
          evidence_ref: fields['evidence_ref'] ?? '',
          ...(fields['sha256'] !== undefined && fields['sha256'] !== ''
            ? { sha256: fields['sha256'] }
            : {}),
          ...(fields['note'] !== undefined && fields['note'] !== ''
            ? { note: fields['note'] }
            : {}),
        });
      }
      return transitionCase(adminApi, selectedCase, fields['status'] ?? '', fields['outcome']);
    },
    onSuccess: () => {
      setNotice(
        caseAction === 'evidence'
          ? `Evidence appended to case #${selectedCase}.`
          : `Case #${selectedCase} transitioned.`,
      );
      invalidate();
    },
    onError: onErr,
  });
  const coiMut = useMutation({
    mutationFn: () =>
      declareConflict(adminApi, {
        ...(coi.memberId !== '' ? { member_id: Number(coi.memberId) } : {}),
        ...(coi.officerId !== '' ? { officer_user_id: Number(coi.officerId) } : {}),
        subject: coi.subject,
        nature: coi.nature,
      }),
    onSuccess: () => {
      setNotice('Conflict of interest declared.');
      invalidate();
    },
    onError: onErr,
  });
  const coiResolveMut = useMutation({
    mutationFn: () =>
      resolveConflict(
        adminApi,
        Number(coiResolve.id),
        coiResolve.status as 'MITIGATED' | 'RECUSED' | 'CLOSED',
        coiResolve.mitigation,
      ),
    onSuccess: () => {
      setNotice(`Conflict #${coiResolve.id} ${coiResolve.status}.`);
      invalidate();
    },
    onError: onErr,
  });

  const denied =
    (interventions.error !== null && isAccessDenied(interventions.error)) ||
    (cases.error !== null && isAccessDenied(cases.error)) ||
    (conflicts.error !== null && isAccessDenied(conflicts.error));
  if (denied) return <AccessDeniedCard />;

  const input = (key: string, ph: string) => (
    <input
      aria-label={`Oversight ${key}`}
      className={inputCls}
      placeholder={ph}
      value={fields[key] ?? ''}
      onChange={(e) => {
        setFields({ ...fields, [key]: e.target.value });
      }}
    />
  );

  return (
    <section className={cardCls} aria-label="Venue oversight">
      <h2 className="mb-2 text-sm font-semibold">Interventions, cases &amp; conflicts</h2>

      <h3 className="mb-1 text-sm font-medium">Market-control interventions</h3>
      {interventions.error !== null ? <ErrorBox error={interventions.error} /> : null}
      {interventions.data !== undefined && interventions.data.length > 0 ? (
        <div className="max-h-36 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Kind</th>
                <th className={thCls}>Scope</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Reason</th>
                <th className={thCls}>
                  <span className="sr-only">Lift</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {interventions.data.map((v) => (
                <tr key={v.interventionId}>
                  <td className={tdCls}>{v.interventionId}</td>
                  <td className={tdCls}>{v.kind}</td>
                  <td className={tdCls}>
                    {v.instrumentId !== undefined
                      ? `instr ${v.instrumentId}`
                      : v.memberId !== undefined
                        ? `member ${v.memberId}`
                        : v.accountId !== undefined
                          ? `acct ${v.accountId}`
                          : 'global'}
                  </td>
                  <td className={tdCls}>
                    <StatusBadge value={v.status || 'ACTIVE'} />
                  </td>
                  <td className={tdCls}>{v.reason}</td>
                  <td className={tdCls}>
                    {v.liftedAt === undefined ? (
                      <button
                        type="button"
                        className={btnGhost}
                        onClick={() => {
                          lift.mutate(v.interventionId);
                        }}
                      >
                        Lift
                      </button>
                    ) : (
                      `lifted ${v.liftedAt.slice(0, 10)}`
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      <form
        aria-label="Record intervention"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (iv.reason !== '') ivMut.mutate();
        }}
      >
        <select
          aria-label="Intervention kind"
          className={selectCls}
          value={iv.kind}
          onChange={(e) => {
            setIv({ ...iv, kind: e.target.value });
          }}
        >
          {INTERVENTION_KINDS.map((k) => (
            <option key={k} value={k}>
              {k}
            </option>
          ))}
        </select>
        {(
          [
            ['reason', 'reason'],
            ['memberId', 'member_id'],
            ['accountId', 'account_id'],
          ] as const
        ).map(([k, ph]) => (
          <input
            key={k}
            aria-label={`IV ${k}`}
            className={inputCls}
            placeholder={ph}
            value={iv[k]}
            onChange={(e) => {
              setIv({ ...iv, [k]: e.target.value });
            }}
          />
        ))}
        <button type="submit" className={btnGhost} disabled={ivMut.isPending}>
          Record
        </button>
      </form>

      <h3 className="mb-1 mt-4 text-sm font-medium">Investigation &amp; disciplinary cases</h3>
      {cases.error !== null ? <ErrorBox error={cases.error} /> : null}
      {cases.data !== undefined && cases.data.length > 0 ? (
        <div className="max-h-36 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Ref</th>
                <th className={thCls}>Kind</th>
                <th className={thCls}>Subject</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Outcome</th>
              </tr>
            </thead>
            <tbody>
              {cases.data.map((c) => (
                <tr
                  key={c.caseId}
                  className="cursor-pointer hover:bg-neutral-800/50"
                  onClick={() => {
                    setSelectedCase(c.caseId);
                  }}
                >
                  <td className={`${tdCls} font-mono text-xs`}>{c.caseRef}</td>
                  <td className={tdCls}>{c.kind}</td>
                  <td className={tdCls}>{c.subject}</td>
                  <td className={tdCls}>
                    <StatusBadge value={c.status || 'OPEN'} />
                  </td>
                  <td className={tdCls}>{c.outcome ?? '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      <form
        aria-label="Open case"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (caseForm.subject !== '') openCase.mutate();
        }}
      >
        <select
          aria-label="Case kind"
          className={selectCls}
          value={caseForm.kind}
          onChange={(e) => {
            setCaseForm({ ...caseForm, kind: e.target.value });
          }}
        >
          <option value="INVESTIGATION">INVESTIGATION</option>
          <option value="DISCIPLINARY">DISCIPLINARY</option>
        </select>
        {(
          [
            ['subject', 'subject'],
            ['memberId', 'member_id'],
            ['accountId', 'account_id'],
          ] as const
        ).map(([k, ph]) => (
          <input
            key={k}
            aria-label={`Case ${k}`}
            className={inputCls}
            placeholder={ph}
            value={caseForm[k]}
            onChange={(e) => {
              setCaseForm({ ...caseForm, [k]: e.target.value });
            }}
          />
        ))}
        <button type="submit" className={btnGhost} disabled={openCase.isPending}>
          Open case
        </button>
      </form>

      {caseDetail.data !== undefined ? (
        <div className="mt-2 rounded border border-neutral-800 p-2">
          <p className="mb-1 text-sm font-medium">
            {caseDetail.data.case.caseRef} — <StatusBadge value={caseDetail.data.case.status} />
          </p>
          {caseDetail.data.evidence.map((ev) => (
            <p key={ev.evidenceId} className="font-mono text-xs text-neutral-400">
              #{ev.evidenceId} · {ev.evidenceRef}
              {ev.sha256 !== undefined ? ` · sha256 ${ev.sha256.slice(0, 12)}…` : ''}
              {ev.note !== undefined ? ` · ${ev.note}` : ''}
            </p>
          ))}
          <form
            aria-label="Case action"
            className="mt-2 flex flex-wrap items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              caseAct.mutate();
            }}
          >
            <select
              aria-label="Case action kind"
              className={selectCls}
              value={caseAction}
              onChange={(e) => {
                setCaseAction(e.target.value as typeof caseAction);
                setFields({});
              }}
            >
              <option value="evidence">attach evidence</option>
              <option value="transition">transition</option>
            </select>
            {caseAction === 'evidence' ? (
              <>
                {input('evidence_ref', 'evidence_ref')}
                {input('sha256', 'sha256')}
                {input('note', 'note')}
              </>
            ) : (
              <>
                <select
                  aria-label="Transition status"
                  className={selectCls}
                  value={fields['status'] ?? ''}
                  onChange={(e) => {
                    setFields({ ...fields, status: e.target.value });
                  }}
                >
                  <option value="">select…</option>
                  {(CASE_TRANSITIONS[caseDetail.data.case.status] ?? []).map((s) => (
                    <option key={s} value={s}>
                      {s}
                    </option>
                  ))}
                </select>
                {TERMINAL.has(fields['status'] ?? '')
                  ? input('outcome', 'outcome (required)')
                  : null}
              </>
            )}
            <button type="submit" className={btnGhost} disabled={caseAct.isPending}>
              Apply
            </button>
          </form>
          <p className={hintTextCls}>
            Evidence is append-only; terminal transitions (SANCTIONED/DISMISSED/CLOSED) require an
            outcome — enforced server-side.
          </p>
        </div>
      ) : null}

      <h3 className="mb-1 mt-4 text-sm font-medium">Conflicts of interest</h3>
      {conflicts.error !== null ? <ErrorBox error={conflicts.error} /> : null}
      {conflicts.data !== undefined && conflicts.data.length > 0 ? (
        <div className="max-h-32 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Party</th>
                <th className={thCls}>Subject</th>
                <th className={thCls}>Nature</th>
                <th className={thCls}>Status</th>
              </tr>
            </thead>
            <tbody>
              {conflicts.data.map((c) => (
                <tr key={c.conflictId}>
                  <td className={tdCls}>{c.conflictId}</td>
                  <td className={tdCls}>
                    {c.memberId !== undefined
                      ? `member ${c.memberId}`
                      : c.officerUserId !== undefined
                        ? `officer ${c.officerUserId}`
                        : '—'}
                  </td>
                  <td className={tdCls}>{c.subject}</td>
                  <td className={tdCls}>{c.nature}</td>
                  <td className={tdCls}>
                    <StatusBadge value={c.status || 'DECLARED'} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      <form
        aria-label="Declare conflict"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (coi.subject !== '' && coi.nature !== '') coiMut.mutate();
        }}
      >
        {(
          [
            ['memberId', 'member_id'],
            ['officerId', 'officer_user_id'],
            ['subject', 'subject'],
            ['nature', 'nature'],
          ] as const
        ).map(([k, ph]) => (
          <input
            key={k}
            aria-label={`COI ${k}`}
            className={inputCls}
            placeholder={ph}
            value={coi[k]}
            onChange={(e) => {
              setCoi({ ...coi, [k]: e.target.value });
            }}
          />
        ))}
        <button type="submit" className={btnGhost} disabled={coiMut.isPending}>
          Declare
        </button>
      </form>
      <form
        aria-label="Resolve conflict"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(coiResolve.id) > 0 && coiResolve.mitigation !== '') coiResolveMut.mutate();
        }}
      >
        <input
          aria-label="COI resolve id"
          className={inputCls}
          placeholder="conflict_id"
          value={coiResolve.id}
          onChange={(e) => {
            setCoiResolve({ ...coiResolve, id: e.target.value });
          }}
        />
        <select
          aria-label="COI resolve status"
          className={selectCls}
          value={coiResolve.status}
          onChange={(e) => {
            setCoiResolve({ ...coiResolve, status: e.target.value });
          }}
        >
          {['MITIGATED', 'RECUSED', 'CLOSED'].map((s) => (
            <option key={s} value={s}>
              {s}
            </option>
          ))}
        </select>
        <input
          aria-label="COI mitigation"
          className={inputCls}
          placeholder="mitigation"
          value={coiResolve.mitigation}
          onChange={(e) => {
            setCoiResolve({ ...coiResolve, mitigation: e.target.value });
          }}
        />
        <button type="submit" className={btnGhost} disabled={coiResolveMut.isPending}>
          Resolve
        </button>
      </form>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
