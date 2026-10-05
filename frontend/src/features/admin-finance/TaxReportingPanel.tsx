/**
 * Tax-reporting runs panel (Phase-10.5 Task 10.5.3.10 §4) — CRS/FATCA
 * report-run lifecycle: DRAFT → UNDER_REVIEW → APPROVED → SUBMITTED.
 * Approve is route-level dual-control (approver must differ from
 * creator/reviewer, enforced claims-side); the integrity-checked XML
 * artifact is linked per run. Rejected runs carry their reason.
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
import { fetchTaxRuns, generateTaxRun, taxRunTransition, type TaxRun } from './api';

const LIFE = ['DRAFT', 'UNDER_REVIEW', 'APPROVED', 'SUBMITTED'];

export function TaxReportingPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [regimeFilter, setRegimeFilter] = useState('');
  const [form, setForm] = useState({ regime: 'CRS', reportYear: '', jurisdiction: '' });
  const [submitRefs, setSubmitRefs] = useState<Record<number, string>>({});
  const [rejectReasons, setRejectReasons] = useState<Record<number, string>>({});
  const [notice, setNotice] = useState<string | null>(null);

  const runs = useQuery({
    queryKey: ['admin-tax-runs', regimeFilter],
    queryFn: () => fetchTaxRuns(adminApi, regimeFilter === '' ? undefined : regimeFilter),
  });
  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-tax-runs'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const generate = useMutation({
    mutationFn: () =>
      generateTaxRun(adminApi, {
        regime: form.regime,
        reportYear: Number(form.reportYear),
        jurisdiction: form.jurisdiction,
      }),
    onSuccess: (r) => {
      setNotice(`Run #${r.id} generated — ${r.status} (${r.accountCount} accounts).`);
      invalidate();
    },
    onError: onErr,
  });
  const transition = useMutation({
    mutationFn: ({
      id,
      verb,
      body,
    }: {
      id: number;
      verb: 'review' | 'approve' | 'reject' | 'submit';
      body?: Record<string, unknown>;
    }) => taxRunTransition(adminApi, id, verb, body),
    onSuccess: (r) => {
      setNotice(`Run #${r.id} → ${r.status}.`);
      invalidate();
    },
    onError: onErr,
  });

  if (runs.error !== null && isAccessDenied(runs.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Tax reporting">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Tax reporting (CRS / FATCA)</h2>
        <input
          aria-label="Regime filter"
          className={inputCls}
          placeholder="regime filter"
          value={regimeFilter}
          onChange={(e) => {
            setRegimeFilter(e.target.value);
          }}
        />
      </div>
      <p className={hintTextCls}>
        Lifecycle DRAFT → UNDER_REVIEW → APPROVED → SUBMITTED — approval is dual-control (approver ≠
        creator/reviewer); XML artifacts are integrity-checked on read.
      </p>

      {runs.error !== null ? <ErrorBox error={runs.error} /> : null}
      {runs.data?.length === 0 ? <p className="text-sm text-neutral-500">No report runs.</p> : null}
      {runs.data !== undefined && runs.data.length > 0 ? (
        <div className="max-h-72 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Regime</th>
                <th className={thCls}>Year</th>
                <th className={thCls}>Jurisdiction</th>
                <th className={thCls}>Accounts</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Lifecycle</th>
                <th className={thCls}>
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {runs.data.map((r) => (
                <RunRow
                  key={r.id}
                  run={r}
                  pending={transition.isPending}
                  submitRef={submitRefs[r.id] ?? ''}
                  rejectReason={rejectReasons[r.id] ?? ''}
                  onSubmitRef={(v) => {
                    setSubmitRefs({ ...submitRefs, [r.id]: v });
                  }}
                  onRejectReason={(v) => {
                    setRejectReasons({ ...rejectReasons, [r.id]: v });
                  }}
                  onTransition={(verb, body) => {
                    transition.mutate({ id: r.id, verb, body });
                  }}
                />
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      <form
        aria-label="Generate tax run"
        className="mt-3 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(form.reportYear) > 1900 && form.jurisdiction !== '') {
            generate.mutate();
          }
        }}
      >
        <select
          aria-label="Regime"
          className={selectCls}
          value={form.regime}
          onChange={(e) => {
            setForm({ ...form, regime: e.target.value });
          }}
        >
          <option value="CRS">CRS</option>
          <option value="FATCA">FATCA</option>
        </select>
        <input
          aria-label="Report year"
          className={inputCls}
          placeholder="report_year"
          value={form.reportYear}
          onChange={(e) => {
            setForm({ ...form, reportYear: e.target.value });
          }}
        />
        <input
          aria-label="Jurisdiction"
          className={inputCls}
          placeholder="jurisdiction"
          value={form.jurisdiction}
          onChange={(e) => {
            setForm({ ...form, jurisdiction: e.target.value });
          }}
        />
        <button type="submit" className={btnPrimary} disabled={generate.isPending}>
          Generate run
        </button>
      </form>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}

function RunRow({
  run,
  pending,
  submitRef,
  rejectReason,
  onSubmitRef,
  onRejectReason,
  onTransition,
}: {
  run: TaxRun;
  pending: boolean;
  submitRef: string;
  rejectReason: string;
  onSubmitRef: (v: string) => void;
  onRejectReason: (v: string) => void;
  onTransition: (
    verb: 'review' | 'approve' | 'reject' | 'submit',
    body?: Record<string, unknown>,
  ) => void;
}) {
  const idx = LIFE.indexOf(run.status);
  return (
    <tr>
      <td className={tdCls}>
        <a className="underline" href={`/api/v1/admin/tax-reporting/runs/${run.id}/xml`}>
          #{run.id}
        </a>
        {run.version > 0 ? ` v${run.version}` : ''}
      </td>
      <td className={tdCls}>{run.regime}</td>
      <td className={tdCls}>{run.reportYear}</td>
      <td className={tdCls}>{run.jurisdiction}</td>
      <td className={tdCls}>{run.accountCount}</td>
      <td className={tdCls}>
        <StatusBadge value={run.status || 'UNKNOWN'} />
        {run.status === 'REJECTED' && run.rejectionReason !== undefined ? (
          <div className="text-xs text-neutral-500">{run.rejectionReason}</div>
        ) : null}
      </td>
      <td className={tdCls}>
        <span className="text-xs" aria-label={`Lifecycle stage ${run.status}`}>
          {LIFE.map((s, i) => (
            <span key={s} className={i <= idx ? 'font-semibold' : 'text-neutral-500'}>
              {s}
              {i < LIFE.length - 1 ? ' → ' : ''}
            </span>
          ))}
        </span>
      </td>
      <td className={tdCls}>
        <div className="flex flex-wrap items-center gap-1">
          {run.status === 'DRAFT' ? (
            <button
              type="button"
              className={btnPrimary}
              disabled={pending}
              onClick={() => {
                onTransition('review');
              }}
            >
              Review
            </button>
          ) : null}
          {run.status === 'UNDER_REVIEW' ? (
            <button
              type="button"
              className={btnPrimary}
              disabled={pending}
              onClick={() => {
                onTransition('approve');
              }}
            >
              Approve (4-eyes)
            </button>
          ) : null}
          {run.status === 'APPROVED' ? (
            <>
              <input
                aria-label={`Submission ref for run ${run.id}`}
                className={inputCls}
                placeholder="submission_ref"
                value={submitRef}
                onChange={(e) => {
                  onSubmitRef(e.target.value);
                }}
              />
              <button
                type="button"
                className={btnPrimary}
                disabled={pending || submitRef === ''}
                onClick={() => {
                  onTransition('submit', { submission_ref: submitRef });
                }}
              >
                Submit
              </button>
            </>
          ) : null}
          {run.status === 'DRAFT' || run.status === 'UNDER_REVIEW' ? (
            <>
              <input
                aria-label={`Reject reason for run ${run.id}`}
                className={inputCls}
                placeholder="reject reason"
                value={rejectReason}
                onChange={(e) => {
                  onRejectReason(e.target.value);
                }}
              />
              <button
                type="button"
                className={btnGhost}
                disabled={pending || rejectReason === ''}
                onClick={() => {
                  onTransition('reject', { reason: rejectReason });
                }}
              >
                Reject
              </button>
            </>
          ) : null}
        </div>
      </td>
    </tr>
  );
}
