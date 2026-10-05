/**
 * AML program panel (Phase-10.5 Task 10.5.3.6 §3) — the FinCEN MSB
 * program register: mandatory-artifact health check (breaches carry
 * MSB_COMPLIANCE_BREACH), artifact register + filing, and the
 * rule-detection monitoring feed with suspicion scores.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnPrimary,
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
  ARTIFACT_TYPES,
  fetchAmlArtifacts,
  fetchAmlMonitoring,
  fetchAmlProgram,
  registerAmlArtifact,
} from './api';

export function AmlPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [acctFilter, setAcctFilter] = useState('');
  const [notice, setNotice] = useState<string | null>(null);
  const [form, setForm] = useState({
    artifactType: 'POLICY',
    title: '',
    reference: '',
    version: '',
    reviewDueAt: '',
  });

  const program = useQuery({
    queryKey: ['admin-aml-program'],
    queryFn: () => fetchAmlProgram(adminApi),
  });
  const artifacts = useQuery({
    queryKey: ['admin-aml-artifacts'],
    queryFn: () => fetchAmlArtifacts(adminApi),
  });
  const monitoring = useQuery({
    queryKey: ['admin-aml-monitoring', acctFilter],
    queryFn: () => fetchAmlMonitoring(adminApi, Number(acctFilter)),
  });

  const register = useMutation({
    mutationFn: () =>
      registerAmlArtifact(adminApi, {
        artifactType: form.artifactType,
        title: form.title,
        reference: form.reference === '' ? undefined : form.reference,
        version: form.version === '' ? undefined : form.version,
        reviewDueAt: form.reviewDueAt === '' ? undefined : form.reviewDueAt,
      }),
    onSuccess: () => {
      setNotice('Artifact registered.');
      void qc.invalidateQueries({ queryKey: ['admin-aml-artifacts'] });
      void qc.invalidateQueries({ queryKey: ['admin-aml-program'] });
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Register failed'),
  });

  if (program.error !== null && isAccessDenied(program.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="AML program">
      <h2 className="mb-2 text-sm font-semibold">AML program</h2>

      {program.data !== undefined ? (
        <div className="mb-3 flex flex-wrap items-center gap-2 text-sm">
          <StatusBadge value={program.data.compliant ? 'COMPLIANT' : 'BREACH'} />
          {program.data.code !== undefined ? (
            <span className={hintTextCls}>{program.data.code}</span>
          ) : null}
          {program.data.breaches.map((b) => (
            <span key={b} className="rounded bg-red-500/20 px-2 py-0.5 text-xs text-red-300">
              {b}
            </span>
          ))}
        </div>
      ) : null}
      {program.error !== null ? <ErrorBox error={program.error} /> : null}

      <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
        <div>
          <p className={hintTextCls}>Program artifacts</p>
          {artifacts.data !== undefined && artifacts.data.length > 0 ? (
            <div className="mt-1 max-h-44 overflow-y-auto">
              <table className={tableCls}>
                <thead>
                  <tr>
                    <th className={thCls}>Type</th>
                    <th className={thCls}>Title</th>
                    <th className={thCls}>Version</th>
                    <th className={thCls}>Status</th>
                    <th className={thCls}>Review due</th>
                  </tr>
                </thead>
                <tbody>
                  {artifacts.data.map((a) => (
                    <tr key={a.id}>
                      <td className={tdCls}>{a.artifactType}</td>
                      <td className={tdCls}>{a.title}</td>
                      <td className={tdCls}>{a.version ?? '—'}</td>
                      <td className={tdCls}>
                        <StatusBadge value={a.status || 'UNKNOWN'} />
                      </td>
                      <td className={tdCls}>{a.reviewDueAt ?? '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <p className="mt-1 text-sm text-neutral-500">No artifacts registered.</p>
          )}

          <form
            aria-label="Register artifact"
            className="mt-2 space-y-1"
            onSubmit={(e) => {
              e.preventDefault();
              if (form.title !== '') register.mutate();
            }}
          >
            <div className="flex gap-2">
              <select
                aria-label="Artifact type"
                className={selectCls}
                value={form.artifactType}
                onChange={(e) => {
                  setForm({ ...form, artifactType: e.target.value });
                }}
              >
                {ARTIFACT_TYPES.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
              </select>
              <input
                aria-label="Artifact title"
                className={inputCls}
                placeholder="title"
                value={form.title}
                onChange={(e) => {
                  setForm({ ...form, title: e.target.value });
                }}
              />
            </div>
            <div className="flex gap-2">
              <input
                aria-label="Artifact reference"
                className={inputCls}
                placeholder="reference"
                value={form.reference}
                onChange={(e) => {
                  setForm({ ...form, reference: e.target.value });
                }}
              />
              <input
                aria-label="Artifact version"
                className={inputCls}
                placeholder="version"
                value={form.version}
                onChange={(e) => {
                  setForm({ ...form, version: e.target.value });
                }}
              />
              <input
                aria-label="Review due"
                className={inputCls}
                placeholder="review due RFC3339"
                value={form.reviewDueAt}
                onChange={(e) => {
                  setForm({ ...form, reviewDueAt: e.target.value });
                }}
              />
            </div>
            <button type="submit" className={btnPrimary} disabled={register.isPending}>
              Register artifact
            </button>
            {notice !== null ? <p className="text-sm">{notice}</p> : null}
          </form>
        </div>

        <div>
          <div className="flex items-end justify-between gap-2">
            <p className={hintTextCls}>Monitoring feed (rule detections)</p>
            <div>
              <label className={labelCls} htmlFor="aml-acct">
                Account
              </label>
              <input
                id="aml-acct"
                className={inputCls}
                value={acctFilter}
                onChange={(e) => {
                  setAcctFilter(e.target.value);
                }}
              />
            </div>
          </div>
          {monitoring.error !== null ? <ErrorBox error={monitoring.error} /> : null}
          {monitoring.data !== undefined && monitoring.data.length > 0 ? (
            <div className="mt-1 max-h-44 overflow-y-auto">
              <table className={tableCls}>
                <thead>
                  <tr>
                    <th className={thCls}>Rule</th>
                    <th className={thCls}>Account</th>
                    <th className={thCls}>Date</th>
                    <th className={thCls}>Score</th>
                    <th className={thCls}>SAR</th>
                  </tr>
                </thead>
                <tbody>
                  {monitoring.data.map((m) => (
                    <tr key={m.id}>
                      <td className={tdCls}>{m.ruleId}</td>
                      <td className={tdCls}>{m.accountId}</td>
                      <td className={tdCls}>{m.businessDate}</td>
                      <td className={tdCls}>{m.score}</td>
                      <td className={tdCls}>{m.sarId ?? '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <p className="mt-1 text-sm text-neutral-500">No monitoring events.</p>
          )}
        </div>
      </div>
    </section>
  );
}
