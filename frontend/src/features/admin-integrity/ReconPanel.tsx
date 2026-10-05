/**
 * Reconciliation panel (Phase-10.5 Task 10.5.3.3 §2) — the Phase-13
 * Task 13.3.2 read surface: latest run + run history + per-run finding
 * drill-down with the mismatch/inconclusive/halt breakdown
 * (migration-286 semantics). INCONCLUSIVE legs are honest "cannot
 * verify" markers — rendered as such, never as clean.
 */
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnGhost,
  cardCls,
  ErrorBox,
  hintTextCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { AccessDeniedCard } from '../admin/RequireAdmin';
import { isAccessDenied } from '../admin/adminRole';
import {
  fetchReconLatest,
  fetchReconRunFindings,
  fetchReconRuns,
  renderScalar,
  type ReconFinding,
} from './api';

function FindingsTable({ findings }: { findings: ReconFinding[] }) {
  if (findings.length === 0) {
    return <p className="text-sm text-neutral-500">No findings — the run verified clean.</p>;
  }
  return (
    <div className="max-h-72 overflow-y-auto">
      <table className={tableCls}>
        <thead>
          <tr>
            <th className={thCls}>Severity</th>
            <th className={thCls}>Category</th>
            <th className={thCls}>Subject</th>
            <th className={thCls}>Leg</th>
            <th className={thCls}>Expected</th>
            <th className={thCls}>Actual</th>
            <th className={thCls}>Delta</th>
            <th className={thCls}>Halt</th>
          </tr>
        </thead>
        <tbody>
          {findings.map((f, i) => (
            <tr key={f.id || i}>
              <td className={tdCls}>
                <StatusBadge value={f.severity || 'UNKNOWN'} />
              </td>
              <td className={tdCls}>{f.category}</td>
              <td className={tdCls} title={f.subject}>
                {f.subject}
              </td>
              <td className={tdCls}>{f.leg}</td>
              <td className={tdCls}>{renderScalar(f.expected)}</td>
              <td className={tdCls}>{renderScalar(f.actual)}</td>
              <td className={tdCls}>{renderScalar(f.delta)}</td>
              <td className={tdCls}>
                {f.haltScope !== undefined ? `${f.haltScope}:${f.haltTarget ?? ''}` : '—'}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function ReconPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [selectedRun, setSelectedRun] = useState<number | null>(null);

  const latest = useQuery({
    queryKey: ['admin-int', 'recon-latest', adminApi.env],
    queryFn: () => fetchReconLatest(adminApi),
    retry: false,
    refetchInterval: 60_000,
  });
  const runs = useQuery({
    queryKey: ['admin-int', 'recon-runs', adminApi.env],
    queryFn: () => fetchReconRuns(adminApi, 20),
    retry: false,
    refetchInterval: 60_000,
  });
  const drill = useQuery({
    queryKey: ['admin-int', 'recon-findings', adminApi.env, selectedRun],
    queryFn: () => fetchReconRunFindings(adminApi, selectedRun ?? 0),
    retry: false,
    enabled: selectedRun !== null,
  });

  if (isAccessDenied(latest.error) || isAccessDenied(runs.error)) {
    return <AccessDeniedCard detail="Reconciliation evidence requires an auditor-capable role." />;
  }

  const findings = selectedRun !== null ? (drill.data ?? []) : (latest.data?.findings ?? []);

  return (
    <section className={cardCls} aria-label="Reconciliation">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">Reconciliation</h2>
      <p className={hintTextCls}>
        Engine↔PG↔projections divergence sweeps. MISMATCH findings emit protective halts;
        INCONCLUSIVE legs are unverifiable seams, not failures.
      </p>

      {latest.isError && <ErrorBox error={latest.error} />}
      {latest.isSuccess &&
        (latest.data.run === null ? (
          <p className="mt-2 text-sm text-neutral-500">
            No reconciliation run recorded yet — the engine has not completed a sweep.
          </p>
        ) : (
          <div className="mt-2 flex flex-wrap items-center gap-3 text-sm">
            <StatusBadge value={latest.data.run.status} />
            <span className="text-neutral-300">
              run #{latest.data.run.id} — {latest.data.run.startedAt}
            </span>
            <span className="text-neutral-400">
              {latest.data.run.categoriesChecked} categories · {latest.data.run.mismatchCount}{' '}
              mismatch · {latest.data.run.inconclusiveCount} inconclusive ·{' '}
              {latest.data.run.halts.length} halts
            </span>
            {latest.data.run.halts.map((h, i) => (
              <span key={i} className="text-xs text-red-400">
                HALT {h.scope}:{h.target} — {h.reason}
              </span>
            ))}
          </div>
        ))}

      <div className="mt-3 grid gap-3 lg:grid-cols-3">
        <div>
          <h3 className="mb-1 text-xs font-semibold uppercase tracking-wide text-neutral-500">
            Recent runs
          </h3>
          {runs.isError && <ErrorBox error={runs.error} />}
          {runs.isSuccess &&
            (runs.data.length === 0 ? (
              <p className="text-sm text-neutral-500">No runs.</p>
            ) : (
              <ul className="max-h-60 space-y-1 overflow-y-auto text-sm">
                {runs.data.map((r) => (
                  <li key={r.id}>
                    <button
                      type="button"
                      className={btnGhost}
                      aria-pressed={selectedRun === r.id}
                      onClick={() => setSelectedRun((cur) => (cur === r.id ? null : r.id))}
                    >
                      #{r.id} {r.status} — {r.startedAt.slice(0, 19)}
                    </button>
                  </li>
                ))}
              </ul>
            ))}
        </div>
        <div className="lg:col-span-2">
          <h3 className="mb-1 text-xs font-semibold uppercase tracking-wide text-neutral-500">
            {selectedRun !== null ? `Findings — run #${selectedRun}` : 'Latest findings'}
          </h3>
          {drill.isError && <ErrorBox error={drill.error} />}
          <FindingsTable findings={findings} />
        </div>
      </div>
    </section>
  );
}
