/**
 * Async export jobs (Task 10.5.3.22 item 2) — folded into the Reports
 * download center. `GET /export-jobs` lists the caller's own jobs
 * (§8.8 envelope, owner-scoped); `GET /export-jobs/{id}/download`
 * streams the artifact while its 24h link is live — the server never
 * exposes a download_url for an expired job, and neither do we.
 */
import { useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { downloadFile, saveBlob } from '@/lib/input-helpers';
import { ErrorBox, btnGhost, tableCls, tdCls, thCls, useNow } from '@/lib/ui';

import { fetchExportJob, fetchExportJobs, type ExportJob } from '@/features/explorer/api';

const ACTIVE = new Set(['QUEUED', 'RUNNING', 'PENDING']);

function JobRow({ job, onRefresh }: { job: ExportJob; onRefresh: (j: ExportJob) => void }) {
  const now = useNow(1000);
  const [err, setErr] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const expiresMs = job.expires_at !== undefined ? Date.parse(job.expires_at) : NaN;
  const active = job.status !== undefined && ACTIVE.has(job.status);
  return (
    <tr>
      <td className={`${tdCls} font-mono`}>#{job.id}</td>
      <td className={`${tdCls} font-mono`}>
        {job.kind ?? '—'}
        {job.interval !== undefined && job.interval !== '' ? ` · ${job.interval}` : ''}
      </td>
      <td className={`${tdCls} font-mono`}>{job.symbol ?? '—'}</td>
      <td className={`${tdCls} font-mono`}>{job.format ?? '—'}</td>
      <td className={tdCls}>
        {job.status ?? '—'}
        {job.truncated === true ? (
          <span className="ml-1 rounded bg-amber-500/15 px-1 text-[10px] text-amber-400">
            truncated
          </span>
        ) : null}
      </td>
      <td className={`${tdCls} font-mono`}>{job.row_count ?? '—'}</td>
      <td className={tdCls}>
        {job.error !== undefined && job.error !== '' ? (
          <span className="text-red-400">{job.error}</span>
        ) : !Number.isNaN(expiresMs) ? (
          expiresMs > now ? (
            `expires in ${Math.max(0, Math.round((expiresMs - now) / 60000))}m`
          ) : (
            'expired'
          )
        ) : (
          '—'
        )}
      </td>
      <td className={tdCls}>
        {job.download_url !== undefined ? (
          <button
            type="button"
            className={btnGhost}
            disabled={busy}
            onClick={() => {
              setBusy(true);
              setErr(null);
              void downloadFile(
                `/export-jobs/${job.id}/download`,
                `export-${job.id}.${job.format ?? 'csv'}`,
              )
                .then(saveBlob)
                .catch(setErr)
                .finally(() => {
                  setBusy(false);
                });
            }}
          >
            Download
          </button>
        ) : active ? (
          <button
            type="button"
            className={btnGhost}
            disabled={busy}
            onClick={() => {
              setBusy(true);
              setErr(null);
              void fetchExportJob(job.id)
                .then((j) => {
                  if (j !== null) onRefresh(j);
                })
                .catch(setErr)
                .finally(() => {
                  setBusy(false);
                });
            }}
          >
            Refresh
          </button>
        ) : null}
        {err !== null ? <ErrorBox error={err} /> : null}
      </td>
    </tr>
  );
}

export function ExportJobsPanel() {
  const qc = useQueryClient();
  const [pages, setPages] = useState<{ rows: ExportJob[]; next: string | null }[]>([]);
  const [moreErr, setMoreErr] = useState<unknown>(null);
  const q = useQuery({
    queryKey: ['export-jobs'],
    queryFn: () => fetchExportJobs(undefined, apiClient),
    // Poll while any job is still QUEUED/RUNNING so create→complete is
    // visible without a manual refresh.
    refetchInterval: (query) =>
      (query.state.data?.rows ?? []).some((j) => j.status !== undefined && ACTIVE.has(j.status))
        ? 3000
        : false,
  });

  const jobs = [...(q.data?.rows ?? []), ...pages.flatMap((p) => p.rows)];
  const lastPage = pages.length > 0 ? pages[pages.length - 1] : undefined;
  const nextCursor = lastPage !== undefined ? lastPage.next : (q.data?.nextCursor ?? null);

  return (
    <section aria-label="Export jobs" className="col-span-full space-y-2">
      <h2 className="text-sm font-semibold text-neutral-100">Export jobs</h2>
      <p className="text-xs text-neutral-500">
        Async exports queued from the history explorer land here. Download links live 24h and are
        owner-scoped — an expired link is a miss, not a stale ref.
      </p>
      {q.isPending ? (
        <p className="text-xs text-neutral-500">Loading…</p>
      ) : q.isError ? (
        <ErrorBox error={q.error} />
      ) : jobs.length === 0 ? (
        <p className="rounded border border-neutral-800 p-4 text-center text-xs text-neutral-500">
          No export jobs yet — queue one from the history explorer.
        </p>
      ) : (
        <div className="relative overflow-x-auto" tabIndex={0}>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Job</th>
                <th className={thCls}>Kind</th>
                <th className={thCls}>Symbol</th>
                <th className={thCls}>Format</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Rows</th>
                <th className={thCls}>Expiry</th>
                <th className={thCls}>
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {jobs.map((j) => (
                <JobRow
                  key={j.id}
                  job={j}
                  onRefresh={() => void qc.invalidateQueries({ queryKey: ['export-jobs'] })}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}
      {moreErr !== null ? <ErrorBox error={moreErr} /> : null}
      {nextCursor !== null ? (
        <button
          type="button"
          className={btnGhost}
          onClick={() => {
            setMoreErr(null);
            void fetchExportJobs(nextCursor, apiClient)
              .then((p) => {
                setPages((s) => [...s, { rows: p.rows, next: p.nextCursor }]);
              })
              .catch(setMoreErr);
          }}
        >
          Load older jobs
        </button>
      ) : null}
    </section>
  );
}
