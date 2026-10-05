/**
 * Public system status (Task 10.5.3.23) — no auth required.
 *
 *   GET /session/status       24/5 session machine (state, coverage,
 *                             market_open, next transition)
 *   GET /system/incidents     public incident notices
 *   GET /maintenance/schedule upcoming SCHEDULED/IN_PROGRESS windows
 */
import { useQuery } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError } from '@/lib/api';
import { ErrorBox, cardCls, tableCls, tdCls, thCls } from '@/lib/ui';

import { fetchIncidents, fetchMaintenance, fetchSessionStatus } from './api';

const retryPublic = (n: number, e: unknown) => !(e instanceof ApiError && e.status < 500) && n < 2;

const fmtTs = (iso?: string) =>
  iso !== undefined && iso !== '' ? new Date(iso).toLocaleString('en-US', { hour12: false }) : '—';

const SEV_STYLE: Record<string, string> = {
  critical: 'text-red-400',
  major: 'text-red-400',
  minor: 'text-amber-400',
  maintenance: 'text-sky-400',
};

function SessionCard() {
  const q = useQuery({
    queryKey: ['public', 'session-status'],
    queryFn: () => fetchSessionStatus(apiClient),
    refetchInterval: 30_000,
    retry: retryPublic,
  });
  return (
    <section aria-label="Trading session" className={cardCls}>
      <h2 className="mb-1 text-sm font-semibold">Trading session</h2>
      {q.isPending ? (
        <p className="text-xs text-neutral-500">Loading…</p>
      ) : q.isError ? (
        <ErrorBox error={q.error} />
      ) : (
        <dl className="grid grid-cols-2 gap-x-4 gap-y-1 text-xs">
          <dt className="text-neutral-500">State</dt>
          <dd className="font-mono">{q.data.state ?? '—'}</dd>
          <dt className="text-neutral-500">Market open</dt>
          <dd
            className={`font-mono ${q.data.market_open === true ? 'text-emerald-400' : 'text-neutral-300'}`}
          >
            {q.data.market_open === true ? 'YES' : 'NO'}
          </dd>
          <dt className="text-neutral-500">Shard coverage</dt>
          <dd className="font-mono">
            {q.data.shard_coverage ?? '—'}
            {q.data.consistent === false ? (
              <span className="ml-1 rounded bg-amber-500/15 px-1 text-[10px] text-amber-400">
                inconsistent
              </span>
            ) : null}
          </dd>
          <dt className="text-neutral-500">Next transition</dt>
          <dd className="font-mono">
            {q.data.next_state ?? '—'} at {fmtTs(q.data.next_transition_at)}
          </dd>
          {(q.data.pending_effects ?? []).length > 0 ? (
            <>
              <dt className="text-neutral-500">Pending effects</dt>
              <dd className="font-mono text-amber-400">
                {(q.data.pending_effects ?? []).join(', ')}
              </dd>
            </>
          ) : null}
        </dl>
      )}
    </section>
  );
}

function IncidentsCard() {
  const q = useQuery({
    queryKey: ['public', 'incidents'],
    queryFn: () => fetchIncidents(apiClient),
    refetchInterval: 60_000,
    retry: retryPublic,
  });
  return (
    <section aria-label="Incidents" className={cardCls}>
      <h2 className="mb-1 text-sm font-semibold">Incidents</h2>
      {q.isPending ? (
        <p className="text-xs text-neutral-500">Loading…</p>
      ) : q.isError ? (
        <ErrorBox error={q.error} />
      ) : q.data.length === 0 ? (
        <p className="text-xs text-neutral-500">No reported incidents.</p>
      ) : (
        <ul className="space-y-2">
          {q.data.map((i) => (
            <li key={i.id} className="rounded border border-neutral-800 p-2 text-xs">
              <div className="flex flex-wrap items-baseline gap-2">
                <span
                  className={`font-medium ${SEV_STYLE[i.severity ?? ''] ?? 'text-neutral-300'}`}
                >
                  {i.severity ?? '—'}
                </span>
                <span className="font-medium text-neutral-200">
                  {i.title ?? `Incident #${i.id}`}
                </span>
                <span className="text-neutral-500">{i.status ?? ''}</span>
                <span className="ml-auto font-mono text-neutral-500">
                  {fmtTs(i.started_at)}
                  {i.resolved_at !== undefined ? ` → ${fmtTs(i.resolved_at)}` : ''}
                </span>
              </div>
              {i.summary !== undefined && i.summary !== '' ? (
                <p className="mt-1 text-neutral-400">{i.summary}</p>
              ) : null}
              {i.mode !== undefined ? (
                <p className="mt-1 text-neutral-500">
                  degradation mode during incident: <span className="font-mono">{i.mode}</span>
                </p>
              ) : null}
              {i.postmortem_url !== undefined ? (
                <a
                  href={i.postmortem_url}
                  className="mt-1 inline-block text-sky-400 hover:underline"
                  rel="noreferrer"
                >
                  Postmortem
                </a>
              ) : null}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function MaintenanceCard() {
  const q = useQuery({
    queryKey: ['public', 'maintenance'],
    queryFn: () => fetchMaintenance(apiClient),
    refetchInterval: 60_000,
    retry: retryPublic,
  });
  return (
    <section aria-label="Maintenance schedule" className={cardCls}>
      <h2 className="mb-1 text-sm font-semibold">Scheduled maintenance</h2>
      {q.isPending ? (
        <p className="text-xs text-neutral-500">Loading…</p>
      ) : q.isError ? (
        <ErrorBox error={q.error} />
      ) : q.data.length === 0 ? (
        <p className="text-xs text-neutral-500">No upcoming maintenance windows.</p>
      ) : (
        <div className="overflow-x-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Window</th>
                <th className={thCls}>Scope</th>
                <th className={thCls}>Symbols</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Starts</th>
                <th className={thCls}>Ends</th>
              </tr>
            </thead>
            <tbody>
              {q.data.map((w) => (
                <tr key={w.id}>
                  <td className={tdCls}>
                    {w.title ?? `#${w.id}`}
                    {w.description !== undefined && w.description !== '' ? (
                      <p className="text-xs text-neutral-500">{w.description}</p>
                    ) : null}
                  </td>
                  <td className={`${tdCls} font-mono`}>{w.scope ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>
                    {(w.symbols ?? []).length > 0 ? (w.symbols ?? []).join(', ') : '—'}
                  </td>
                  <td className={tdCls}>{w.status ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{fmtTs(w.starts_at)}</td>
                  <td className={`${tdCls} font-mono`}>{fmtTs(w.ends_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

export default function StatusPage() {
  return (
    <div className="mx-auto max-w-6xl space-y-4 p-4">
      <header>
        <h1 className="text-xl font-semibold text-neutral-100">System status</h1>
        <p className="text-sm text-neutral-500">
          Public session state, incident notices and the maintenance calendar — no sign-in required.
        </p>
      </header>
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <SessionCard />
        <IncidentsCard />
      </div>
      <MaintenanceCard />
    </div>
  );
}
