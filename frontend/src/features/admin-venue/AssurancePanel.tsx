/**
 * Venue assurance panel (Phase-10.5 Task 10.5.3.14 §2 continued) —
 * annual system-safeguard self-assessments (file → assessor sign-off
 * COMPLETED), the CCO annual report pipeline (generate → board sign →
 * regulator filing), and the production launch gate (prerequisite
 * checklist + ready/missing evaluation).
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
import {
  ccoAction,
  completeAssessment,
  evidencePrerequisite,
  expirePrerequisite,
  fetchAssessments,
  fetchCCOReports,
  fetchLaunchGate,
  fetchPrerequisites,
  fileAssessment,
  generateCCOReport,
} from './api';

export function AssurancePanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [assess, setAssess] = useState({ year: '', exceptions: '', remediation: '' });
  const [cco, setCco] = useState({ start: '', end: '' });
  const [ccoFile, setCcoFile] = useState({ id: '', ref: '' });
  const [prereq, setPrereq] = useState({
    kind: 'LICENSING',
    scope: '',
    evidenceRef: '',
    expiresAt: '',
  });
  const [notice, setNotice] = useState<string | null>(null);

  const assessments = useQuery({
    queryKey: ['admin-venue-assessments'],
    queryFn: () => fetchAssessments(adminApi),
  });
  const reports = useQuery({
    queryKey: ['admin-venue-cco'],
    queryFn: () => fetchCCOReports(adminApi),
  });
  const prereqs = useQuery({
    queryKey: ['admin-venue-prereqs'],
    queryFn: () => fetchPrerequisites(adminApi),
  });
  const gate = useQuery({
    queryKey: ['admin-venue-launch-gate'],
    queryFn: () => fetchLaunchGate(adminApi),
  });
  const invalidate = () => {
    for (const k of [
      'admin-venue-assessments',
      'admin-venue-cco',
      'admin-venue-prereqs',
      'admin-venue-launch-gate',
    ]) {
      void qc.invalidateQueries({ queryKey: [k] });
    }
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const assessMut = useMutation({
    mutationFn: () =>
      fileAssessment(adminApi, {
        period_year: Number(assess.year),
        ...(assess.exceptions !== '' ? { exceptions: { note: assess.exceptions } } : {}),
        ...(assess.remediation !== '' ? { remediation: { note: assess.remediation } } : {}),
      }),
    onSuccess: () => {
      setNotice(`Self-assessment for ${assess.year} filed (control evidence assembled).`);
      invalidate();
    },
    onError: onErr,
  });
  const assessComplete = useMutation({
    mutationFn: (id: number) => completeAssessment(adminApi, id),
    onSuccess: () => {
      setNotice('Assessor sign-off recorded (DRAFT → COMPLETED).');
      invalidate();
    },
    onError: onErr,
  });
  const ccoGen = useMutation({
    mutationFn: () => generateCCOReport(adminApi, { period_start: cco.start, period_end: cco.end }),
    onSuccess: () => {
      setNotice('CCO report generated (DRAFT).');
      invalidate();
    },
    onError: onErr,
  });
  const ccoSign = useMutation({
    mutationFn: (id: number) => ccoAction(adminApi, id, 'sign', {}),
    onSuccess: () => {
      setNotice('Board sign-off recorded (DRAFT → SIGNED).');
      invalidate();
    },
    onError: onErr,
  });
  const ccoFileMut = useMutation({
    mutationFn: () => ccoAction(adminApi, Number(ccoFile.id), 'file', { filing_ref: ccoFile.ref }),
    onSuccess: () => {
      setNotice(`CCO report #${ccoFile.id} filed with the regulator.`);
      invalidate();
    },
    onError: onErr,
  });
  const prereqMut = useMutation({
    mutationFn: () =>
      evidencePrerequisite(adminApi, {
        kind: prereq.kind,
        ...(prereq.scope !== '' ? { scope: prereq.scope } : {}),
        evidence_ref: prereq.evidenceRef,
        ...(prereq.expiresAt !== '' ? { expires_at: prereq.expiresAt } : {}),
      }),
    onSuccess: () => {
      setNotice('Prerequisite evidenced (upsert on kind/scope).');
      invalidate();
    },
    onError: onErr,
  });
  const expireMut = useMutation({
    mutationFn: (id: number) => expirePrerequisite(adminApi, id),
    onSuccess: () => {
      setNotice('Prerequisite evidence marked expired.');
      invalidate();
    },
    onError: onErr,
  });

  const denied =
    (assessments.error !== null && isAccessDenied(assessments.error)) ||
    (reports.error !== null && isAccessDenied(reports.error)) ||
    (gate.error !== null && isAccessDenied(gate.error));
  if (denied) return <AccessDeniedCard />;

  return (
    <section className={cardCls} aria-label="Venue assurance">
      <h2 className="mb-2 text-sm font-semibold">Assessments, CCO reports &amp; launch gate</h2>

      <h3 className="mb-1 text-sm font-medium">Annual self-assessments (RTS 7 / SEF)</h3>
      {assessments.error !== null ? <ErrorBox error={assessments.error} /> : null}
      {assessments.data !== undefined && assessments.data.length > 0 ? (
        <div className="max-h-32 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Year</th>
                <th className={thCls}>v</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>
                  <span className="sr-only">Complete</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {assessments.data.map((a) => (
                <tr key={a.assessmentId}>
                  <td className={tdCls}>{a.assessmentId}</td>
                  <td className={tdCls}>{a.periodYear}</td>
                  <td className={tdCls}>{a.version}</td>
                  <td className={tdCls}>
                    <StatusBadge value={a.status || 'DRAFT'} />
                  </td>
                  <td className={tdCls}>
                    {a.status !== 'COMPLETED' ? (
                      <button
                        type="button"
                        className={btnGhost}
                        onClick={() => {
                          assessComplete.mutate(a.assessmentId);
                        }}
                      >
                        Sign off
                      </button>
                    ) : (
                      (a.assessedAt?.slice(0, 10) ?? 'COMPLETED')
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      <form
        aria-label="File assessment"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(assess.year) >= 2000) assessMut.mutate();
        }}
      >
        {(
          [
            ['year', 'period_year'],
            ['exceptions', 'exceptions'],
            ['remediation', 'remediation'],
          ] as const
        ).map(([k, ph]) => (
          <input
            key={k}
            aria-label={`Assessment ${k}`}
            className={inputCls}
            placeholder={ph}
            value={assess[k]}
            onChange={(e) => {
              setAssess({ ...assess, [k]: e.target.value });
            }}
          />
        ))}
        <button type="submit" className={btnGhost} disabled={assessMut.isPending}>
          File year
        </button>
      </form>

      <h3 className="mb-1 mt-4 text-sm font-medium">CCO annual reports</h3>
      {reports.error !== null ? <ErrorBox error={reports.error} /> : null}
      {reports.data !== undefined && reports.data.length > 0 ? (
        <div className="max-h-32 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Period</th>
                <th className={thCls}>v</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Filing</th>
                <th className={thCls}>
                  <span className="sr-only">Sign</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {reports.data.map((r) => (
                <tr key={r.id}>
                  <td className={tdCls}>{r.id}</td>
                  <td className={tdCls}>
                    {r.periodStart.slice(0, 10)} → {r.periodEnd.slice(0, 10)}
                  </td>
                  <td className={tdCls}>{r.version}</td>
                  <td className={tdCls}>
                    <StatusBadge value={r.status || 'DRAFT'} />
                  </td>
                  <td className={tdCls}>{r.regulatorFilingRef ?? '—'}</td>
                  <td className={tdCls}>
                    {r.status === 'DRAFT' ? (
                      <button
                        type="button"
                        className={btnGhost}
                        onClick={() => {
                          ccoSign.mutate(r.id);
                        }}
                      >
                        Board sign
                      </button>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      <form
        aria-label="Generate CCO report"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (cco.start !== '' && cco.end !== '') ccoGen.mutate();
        }}
      >
        <input
          aria-label="CCO period start"
          className={inputCls}
          type="date"
          value={cco.start}
          onChange={(e) => {
            setCco({ ...cco, start: e.target.value });
          }}
        />
        <input
          aria-label="CCO period end"
          className={inputCls}
          type="date"
          value={cco.end}
          onChange={(e) => {
            setCco({ ...cco, end: e.target.value });
          }}
        />
        <button type="submit" className={btnGhost} disabled={ccoGen.isPending}>
          Generate
        </button>
        <input
          aria-label="CCO file id"
          className={inputCls}
          placeholder="report_id"
          value={ccoFile.id}
          onChange={(e) => {
            setCcoFile({ ...ccoFile, id: e.target.value });
          }}
        />
        <input
          aria-label="CCO filing ref"
          className={inputCls}
          placeholder="filing_ref"
          value={ccoFile.ref}
          onChange={(e) => {
            setCcoFile({ ...ccoFile, ref: e.target.value });
          }}
        />
        <button
          type="button"
          className={btnGhost}
          disabled={ccoFileMut.isPending || Number(ccoFile.id) <= 0 || ccoFile.ref === ''}
          onClick={() => {
            ccoFileMut.mutate();
          }}
        >
          File with regulator
        </button>
      </form>

      <h3 className="mb-1 mt-4 text-sm font-medium">Launch gate</h3>
      {gate.error !== null ? <ErrorBox error={gate.error} /> : null}
      {gate.data !== undefined ? (
        <p className="mb-1 text-sm">
          Gate: <StatusBadge value={gate.data.ready ? 'READY' : 'BLOCKED'} />
          {!gate.data.ready ? ` · ${gate.data.missing.length} missing prerequisite(s)` : ''}
          {` · evaluated ${gate.data.evaluatedAt.slice(0, 16)}`}
        </p>
      ) : null}
      {gate.data !== undefined && gate.data.missing.length > 0 ? (
        <p className={hintTextCls}>
          Missing:{' '}
          {gate.data.missing
            .map((m) => `${m.kind}${m.scope !== '' ? `/${m.scope}` : ''}`)
            .join(', ')}
        </p>
      ) : null}
      {prereqs.data !== undefined && prereqs.data.length > 0 ? (
        <div className="mt-1 max-h-32 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Kind</th>
                <th className={thCls}>Scope</th>
                <th className={thCls}>Required</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Expires</th>
                <th className={thCls}>
                  <span className="sr-only">Expire</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {prereqs.data.map((p) => (
                <tr key={p.prereqId}>
                  <td className={tdCls}>{p.kind}</td>
                  <td className={tdCls}>{p.scope}</td>
                  <td className={tdCls}>{p.required ? 'REQUIRED' : '—'}</td>
                  <td className={tdCls}>
                    <StatusBadge value={p.status || 'PENDING'} />
                  </td>
                  <td className={tdCls}>{p.expiresAt?.slice(0, 10) ?? '—'}</td>
                  <td className={tdCls}>
                    <button
                      type="button"
                      className={btnGhost}
                      onClick={() => {
                        expireMut.mutate(p.prereqId);
                      }}
                    >
                      Expire
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      <form
        aria-label="Evidence prerequisite"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (prereq.evidenceRef !== '') prereqMut.mutate();
        }}
      >
        <select
          aria-label="Prereq kind"
          className={selectCls}
          value={prereq.kind}
          onChange={(e) => {
            setPrereq({ ...prereq, kind: e.target.value });
          }}
        >
          {['LICENSING', 'REGULATOR_AUTH', 'LEGAL_OPINION', 'BOARD_CCO', 'FINANCIAL_RESOURCES'].map(
            (k) => (
              <option key={k} value={k}>
                {k}
              </option>
            ),
          )}
        </select>
        {(
          [
            ['scope', 'scope'],
            ['evidenceRef', 'evidence_ref'],
            ['expiresAt', 'expires_at'],
          ] as const
        ).map(([k, ph]) => (
          <input
            key={k}
            aria-label={`Prereq ${k}`}
            className={inputCls}
            placeholder={ph}
            value={prereq[k]}
            onChange={(e) => {
              setPrereq({ ...prereq, [k]: e.target.value });
            }}
          />
        ))}
        <button type="submit" className={btnPrimary} disabled={prereqMut.isPending}>
          Evidence
        </button>
      </form>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
