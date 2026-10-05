/**
 * Surveillance case desk (Phase-10.5 Task 10.5.3.6 §1) — the
 * Phase-17→21 signal-to-case pipeline UI: monthly effectiveness
 * summary, the case queue with SLA timers, the investigation workspace
 * (immutable evidence, linked signals, order-audit refs) and the three
 * case actions — assign, append evidence, terminal disposition
 * (FALSE_POSITIVE / ESCALATE_SAR / ESCALATE_STR / ESCALATE_ACTION).
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnDanger,
  btnGhost,
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
import {
  assignCase,
  attachEvidence,
  CASE_DISPOSITIONS,
  disposeCase,
  fetchCases,
  fetchCaseWorkspace,
  fetchSurveillanceSummary,
  type SurveillanceCase,
} from './api';

const STATUS_FILTERS = ['', 'OPEN', 'ASSIGNED', 'INVESTIGATING'] as const;

function Workspace({ adminApi, caseId }: { adminApi: BoundAdminApi; caseId: number }) {
  const qc = useQueryClient();
  const [assignee, setAssignee] = useState('');
  const [dispo, setDispo] = useState({ disposition: 'FALSE_POSITIVE', reason: '', action: '' });
  const [ev, setEv] = useState({ kind: 'NOTE', body: '', attachmentRef: '', sha256: '' });
  const [notice, setNotice] = useState<string | null>(null);

  const ws = useQuery({
    queryKey: ['admin-surv-case', caseId],
    queryFn: () => fetchCaseWorkspace(adminApi, caseId),
  });

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['admin-surv-case', caseId] });
    void qc.invalidateQueries({ queryKey: ['admin-surv-cases'] });
  };

  const assign = useMutation({
    mutationFn: () => assignCase(adminApi, caseId, Number(assignee)),
    onSuccess: () => {
      setNotice('Case assigned.');
      refresh();
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Assign failed'),
  });

  const dispose = useMutation({
    mutationFn: () =>
      disposeCase(adminApi, caseId, {
        disposition: dispo.disposition,
        reason: dispo.reason,
        action: dispo.disposition === 'ESCALATE_ACTION' ? dispo.action : undefined,
      }),
    onSuccess: (c) => {
      setNotice(`Case → ${c.status}`);
      refresh();
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Disposition failed'),
  });

  const evidence = useMutation({
    mutationFn: () =>
      attachEvidence(adminApi, caseId, {
        kind: ev.kind,
        body: ev.body,
        attachmentRef: ev.attachmentRef === '' ? undefined : ev.attachmentRef,
        sha256: ev.sha256 === '' ? undefined : ev.sha256,
      }),
    onSuccess: () => {
      setNotice('Evidence attached (append-only).');
      setEv({ ...ev, body: '' });
      void qc.invalidateQueries({ queryKey: ['admin-surv-case', caseId] });
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Evidence failed'),
  });

  if (ws.error !== null) return <ErrorBox error={ws.error} />;
  if (ws.data === undefined) return <p className={hintTextCls}>Loading workspace…</p>;
  const { case: c, evidence: evs, linkedSignals, orderAuditIds } = ws.data;
  const terminal = c.status.startsWith('CLOSED') || c.status.startsWith('ESCALATED');

  return (
    <div
      aria-label="Case workspace"
      className="mt-3 space-y-3 rounded border border-neutral-700 p-3"
    >
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <strong>#{c.id}</strong> <span>{c.caseRef}</span>
        <StatusBadge value={c.status || 'UNKNOWN'} />
        <StatusBadge value={c.severity || 'UNKNOWN'} />
        <span className={hintTextCls}>
          {c.signalType} {c.symbol} · account {c.accountId ?? '—'} · SLA {c.slaDeadline}
          {c.slaBreached ? ' (BREACHED)' : ''}
        </span>
        {c.escalatedSarId !== undefined ? (
          <span className={hintTextCls}>SAR #{c.escalatedSarId}</span>
        ) : null}
      </div>

      <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
        <div>
          <p className={hintTextCls}>Evidence ({evs.length}, append-only)</p>
          <ul className="max-h-36 space-y-1 overflow-y-auto text-sm">
            {evs.map((e) => (
              <li key={e.id} className="rounded border border-neutral-800 p-1">
                <span className="font-mono text-xs">{e.kind}</span> {e.body}
                {e.sha256 !== undefined ? (
                  <span className={hintTextCls}> sha256:{e.sha256.slice(0, 16)}…</span>
                ) : null}
              </li>
            ))}
            {evs.length === 0 ? <li className="text-neutral-500">No evidence yet.</li> : null}
          </ul>
          <p className={`${hintTextCls} mt-2`}>
            Linked signals: {linkedSignals.length > 0 ? linkedSignals.join(', ') : '—'} · order
            audit refs: {orderAuditIds.length > 0 ? orderAuditIds.join(', ') : '—'}
          </p>
        </div>

        <div className="space-y-2">
          <form
            aria-label="Assign case"
            className="flex items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              assign.mutate();
            }}
          >
            <div>
              <label className={labelCls} htmlFor="case-assignee">
                Assign to (0 = round-robin)
              </label>
              <input
                id="case-assignee"
                className={inputCls}
                value={assignee}
                onChange={(e) => {
                  setAssignee(e.target.value);
                }}
              />
            </div>
            <button type="submit" className={btnGhost} disabled={assign.isPending}>
              Assign
            </button>
          </form>

          <form
            aria-label="Attach evidence"
            className="space-y-1 border-t border-neutral-800 pt-2"
            onSubmit={(e) => {
              e.preventDefault();
              if (ev.body !== '') evidence.mutate();
            }}
          >
            <div className="flex gap-2">
              <input
                aria-label="Evidence kind"
                className={inputCls}
                placeholder="kind"
                value={ev.kind}
                onChange={(e) => {
                  setEv({ ...ev, kind: e.target.value });
                }}
              />
              <input
                aria-label="Evidence sha256"
                className={inputCls}
                placeholder="sha256 (optional)"
                value={ev.sha256}
                onChange={(e) => {
                  setEv({ ...ev, sha256: e.target.value });
                }}
              />
            </div>
            <input
              aria-label="Evidence body"
              className={inputCls}
              placeholder="evidence body"
              value={ev.body}
              onChange={(e) => {
                setEv({ ...ev, body: e.target.value });
              }}
            />
            <button type="submit" className={btnGhost} disabled={evidence.isPending}>
              Attach evidence
            </button>
          </form>

          {terminal ? (
            <p className={`${hintTextCls} border-t border-neutral-800 pt-2`}>
              Terminal state reached — the case is immutable.
            </p>
          ) : (
            <form
              aria-label="Dispose case"
              className="space-y-1 border-t border-neutral-800 pt-2"
              onSubmit={(e) => {
                e.preventDefault();
                if (dispo.reason !== '') dispose.mutate();
              }}
            >
              <div className="flex gap-2">
                <select
                  aria-label="Disposition"
                  className={selectCls}
                  value={dispo.disposition}
                  onChange={(e) => {
                    setDispo({ ...dispo, disposition: e.target.value });
                  }}
                >
                  {CASE_DISPOSITIONS.map((d) => (
                    <option key={d} value={d}>
                      {d}
                    </option>
                  ))}
                </select>
                {dispo.disposition === 'ESCALATE_ACTION' ? (
                  <input
                    aria-label="Enforcement action"
                    className={inputCls}
                    placeholder="WARN/THROTTLE/RESTRICT/SUSPEND"
                    value={dispo.action}
                    onChange={(e) => {
                      setDispo({ ...dispo, action: e.target.value });
                    }}
                  />
                ) : null}
              </div>
              <input
                aria-label="Disposition reason"
                className={inputCls}
                placeholder="justification (required)"
                value={dispo.reason}
                onChange={(e) => {
                  setDispo({ ...dispo, reason: e.target.value });
                }}
              />
              <button type="submit" className={btnDanger} disabled={dispose.isPending}>
                Record disposition
              </button>
            </form>
          )}
        </div>
      </div>
      {notice !== null ? <p className="text-sm">{notice}</p> : null}
    </div>
  );
}

export function SurveillancePanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [statusFilter, setStatusFilter] = useState<string>('OPEN');
  const [month, setMonth] = useState('');
  const [selected, setSelected] = useState<SurveillanceCase | null>(null);

  const summary = useQuery({
    queryKey: ['admin-surv-summary', month],
    queryFn: () => fetchSurveillanceSummary(adminApi, month),
  });
  const cases = useQuery({
    queryKey: ['admin-surv-cases', statusFilter],
    queryFn: () => fetchCases(adminApi, statusFilter),
  });

  if (cases.error !== null && isAccessDenied(cases.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Surveillance cases">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Surveillance cases</h2>
        <div className="flex items-end gap-2">
          <div>
            <label className={labelCls} htmlFor="surv-status">
              Status
            </label>
            <select
              id="surv-status"
              className={selectCls}
              value={statusFilter}
              onChange={(e) => {
                setStatusFilter(e.target.value);
              }}
            >
              {STATUS_FILTERS.map((s) => (
                <option key={s} value={s}>
                  {s === '' ? 'All' : s}
                </option>
              ))}
            </select>
          </div>
          <div>
            <label className={labelCls} htmlFor="surv-month">
              Month (YYYY-MM)
            </label>
            <input
              id="surv-month"
              className={inputCls}
              placeholder="current"
              value={month}
              onChange={(e) => {
                setMonth(e.target.value);
              }}
            />
          </div>
        </div>
      </div>

      {summary.data !== undefined ? (
        <div className="mb-2 flex flex-wrap gap-3 text-sm" aria-label="Monthly summary">
          <span className={hintTextCls}>{summary.data.month}:</span>
          <span>opened {summary.data.opened}</span>
          <span>closed {summary.data.closed}</span>
          <span>escalated {summary.data.escalated}</span>
          <span>false-positive {summary.data.falsePositives}</span>
          <span>avg disposition {summary.data.avgDispositionHours.toFixed(1)}h</span>
          {summary.data.openBreached > 0 ? (
            <StatusBadge value={`${summary.data.openBreached} SLA BREACHED`} />
          ) : null}
        </div>
      ) : null}
      {summary.error !== null ? <ErrorBox error={summary.error} /> : null}
      {cases.error !== null ? <ErrorBox error={cases.error} /> : null}
      {cases.data?.length === 0 ? (
        <p className="text-sm text-neutral-500">No cases in this filter.</p>
      ) : null}
      {cases.data !== undefined && cases.data.length > 0 ? (
        <div className="max-h-64 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Signal</th>
                <th className={thCls}>Symbol</th>
                <th className={thCls}>Severity</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Assignee</th>
                <th className={thCls}>SLA</th>
              </tr>
            </thead>
            <tbody>
              {cases.data.map((c) => (
                <tr
                  key={c.id}
                  className="cursor-pointer"
                  onClick={() => {
                    setSelected(c);
                  }}
                >
                  <td className={tdCls} title={c.caseRef}>
                    {c.id}
                  </td>
                  <td className={tdCls}>{c.signalType}</td>
                  <td className={tdCls}>{c.symbol}</td>
                  <td className={tdCls}>
                    <StatusBadge value={c.severity || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>
                    <StatusBadge value={c.status || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>{c.assignedTo ?? '—'}</td>
                  <td className={tdCls}>
                    {c.slaDeadline}
                    {c.slaBreached ? ' !' : ''}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      {selected !== null ? <Workspace adminApi={adminApi} caseId={selected.id} /> : null}
    </section>
  );
}
