/**
 * FX Global Code panel (Phase-10.5 Task 10.5.3.15 §3) — the annual
 * 55-principle self-assessment lifecycle: open run → officer verdicts
 * per principle (PARTIAL/NON_ADHERENT require a remediation ticket) →
 * complete (Statement of Commitment generated; refused while verdicts
 * PENDING) → executive sign-off → publish to the public register.
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
import { fetchFXGCRun, fetchFXGCRuns, fxgcLifecycle, fxgcVerdict, openFXGCRun } from './api';

export function FXGCPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [selectedId, setSelectedId] = useState<number | null>(null);
  const [open, setOpen] = useState({ period: '', codeVersion: '' });
  const [verdict, setVerdict] = useState({
    principleId: '',
    status: 'ADHERENT',
    evidence: '',
    remediation: '',
  });
  const [detail, setDetail] = useState<Record<string, unknown> | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const runs = useQuery({
    queryKey: ['admin-fxgc-runs'],
    queryFn: () => fetchFXGCRuns(adminApi),
  });
  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-fxgc-runs'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const openMut = useMutation({
    mutationFn: () =>
      openFXGCRun(adminApi, {
        period: open.period,
        ...(open.codeVersion !== '' ? { code_version: open.codeVersion } : {}),
      }),
    onSuccess: (id) => {
      setNotice(`Assessment run #${id} opened — 55-principle matrix + automated probes.`);
      invalidate();
    },
    onError: onErr,
  });
  const verdictMut = useMutation({
    mutationFn: () => {
      if (selectedId === null) return Promise.reject(new Error('Select a run'));
      return fxgcVerdict(adminApi, selectedId, {
        principle_id: Number(verdict.principleId),
        adherence_status: verdict.status,
        ...(verdict.evidence !== '' ? { evidence_summary: verdict.evidence } : {}),
        ...(verdict.remediation !== '' ? { remediation_ref: verdict.remediation } : {}),
      });
    },
    onSuccess: () => {
      setNotice(`Verdict recorded on principle ${verdict.principleId}.`);
      invalidate();
    },
    onError: onErr,
  });
  const lifecycle = useMutation({
    mutationFn: (action: 'complete' | 'sign' | 'publish') => {
      if (selectedId === null) return Promise.reject(new Error('Select a run'));
      return fxgcLifecycle(adminApi, selectedId, action);
    },
    onSuccess: (_d, action) => {
      setNotice(`Run #${selectedId} — ${action} applied.`);
      invalidate();
    },
    onError: onErr,
  });
  const loadDetail = useMutation({
    mutationFn: (id: number) => fetchFXGCRun(adminApi, id),
    onSuccess: (d) => {
      setDetail(d);
    },
    onError: onErr,
  });

  if (runs.error !== null && isAccessDenied(runs.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="FX Global Code">
      <h2 className="mb-2 text-sm font-semibold">FX Global Code — 55-principle assessments</h2>
      {runs.error !== null ? <ErrorBox error={runs.error} /> : null}
      {runs.data?.length === 0 ? (
        <p className="text-sm text-neutral-500">No assessment runs.</p>
      ) : null}
      {runs.data !== undefined && runs.data.length > 0 ? (
        <div className="max-h-44 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Run</th>
                <th className={thCls}>Period</th>
                <th className={thCls}>Code</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Adherent/Partial/Non/Pending</th>
              </tr>
            </thead>
            <tbody>
              {runs.data.map((r) => (
                <tr
                  key={r.id}
                  className="cursor-pointer hover:bg-neutral-800/50"
                  onClick={() => {
                    setSelectedId(r.id);
                    loadDetail.mutate(r.id);
                  }}
                >
                  <td className={tdCls}>#{r.id}</td>
                  <td className={tdCls}>{r.period}</td>
                  <td className={tdCls}>{r.codeVersion}</td>
                  <td className={tdCls}>
                    <StatusBadge value={r.status || 'OPEN'} />
                  </td>
                  <td className={tdCls}>
                    {r.principlesAdherent}/{r.principlesPartial}/{r.principlesNon}/
                    {r.principlesPending}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      <form
        aria-label="Open FXGC run"
        className="mt-3 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (open.period !== '') openMut.mutate();
        }}
      >
        <input
          aria-label="Run period"
          className={inputCls}
          placeholder="period (e.g. 2026)"
          value={open.period}
          onChange={(e) => {
            setOpen({ ...open, period: e.target.value });
          }}
        />
        <input
          aria-label="Code version"
          className={inputCls}
          placeholder="code_version"
          value={open.codeVersion}
          onChange={(e) => {
            setOpen({ ...open, codeVersion: e.target.value });
          }}
        />
        <button type="submit" className={btnPrimary} disabled={openMut.isPending}>
          Open run
        </button>
      </form>

      {selectedId !== null ? (
        <div className="mt-3 rounded border border-neutral-800 p-2">
          <p className="mb-1 text-sm font-medium">Run #{selectedId}</p>
          <form
            aria-label="Principle verdict"
            className="flex flex-wrap items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              if (Number(verdict.principleId) > 0) verdictMut.mutate();
            }}
          >
            <input
              aria-label="Principle id"
              className={inputCls}
              placeholder="principle_id (1-55)"
              value={verdict.principleId}
              onChange={(e) => {
                setVerdict({ ...verdict, principleId: e.target.value });
              }}
            />
            <select
              aria-label="Adherence status"
              className={selectCls}
              value={verdict.status}
              onChange={(e) => {
                setVerdict({ ...verdict, status: e.target.value });
              }}
            >
              {['ADHERENT', 'PARTIAL', 'NON_ADHERENT'].map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
            <input
              aria-label="Evidence summary"
              className={inputCls}
              placeholder="evidence_summary"
              value={verdict.evidence}
              onChange={(e) => {
                setVerdict({ ...verdict, evidence: e.target.value });
              }}
            />
            {verdict.status !== 'ADHERENT' ? (
              <input
                aria-label="Remediation ref"
                className={inputCls}
                placeholder="remediation_ref (required)"
                value={verdict.remediation}
                onChange={(e) => {
                  setVerdict({ ...verdict, remediation: e.target.value });
                }}
              />
            ) : null}
            <button type="submit" className={btnGhost} disabled={verdictMut.isPending}>
              Record verdict
            </button>
          </form>
          <div className="mt-2 flex gap-2">
            {(['complete', 'sign', 'publish'] as const).map((a) => (
              <button
                key={a}
                type="button"
                className={btnGhost}
                disabled={lifecycle.isPending}
                onClick={() => {
                  lifecycle.mutate(a);
                }}
              >
                {a === 'complete'
                  ? 'Complete (Statement of Commitment)'
                  : a === 'sign'
                    ? 'Executive sign'
                    : 'Publish to register'}
              </button>
            ))}
          </div>
          <p className={hintTextCls}>
            Completion is refused while any verdict is PENDING; PARTIAL/NON_ADHERENT verdicts
            require a remediation ref.
          </p>
          {detail !== null ? (
            <pre className="mt-2 max-h-48 overflow-auto rounded border border-neutral-800 p-2 font-mono text-xs">
              {JSON.stringify(detail, null, 2)}
            </pre>
          ) : null}
        </div>
      ) : null}
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
