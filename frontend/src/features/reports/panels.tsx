/**
 * Fee schedule / solvency / system-info / announcement panels
 * (Task 10.3.28 items 2–5). Live endpoints render real payloads; stubs
 * render UnavailablePanel.
 */
import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError } from '@/lib/api';
import { ErrorBox, cardCls, tableCls, tdCls, thCls } from '@/lib/ui';
import { UnavailablePanel, isNotImplemented } from '@/lib/input-helpers';

import {
  fetchAnnouncements,
  fetchFees,
  fetchProofRoot,
  fetchSystemStatus,
  type Announcement,
} from './api';

// ---------------------------------------------------------------------------
// Fee schedule
// ---------------------------------------------------------------------------

export function FeeSchedulePanel() {
  const q = useQuery({ queryKey: ['reports', 'fees'], queryFn: () => fetchFees(apiClient) });
  if (q.isPending) return <p className="text-sm text-neutral-500">Loading fee schedule…</p>;
  if (q.isError) return <ErrorBox error={q.error} />;
  const f = q.data;
  return (
    <section aria-label="Fee schedule" className={cardCls}>
      <h2 className="text-base font-semibold text-neutral-100">Fee schedule</h2>
      <p className="mt-1 text-xs text-neutral-500">
        Tier: {f.tierName ?? `#${f.tierId ?? '—'}`} {f.promoActive ? '· promo active' : ''}
      </p>
      <table className={tableCls + ' mt-3 max-w-md'}>
        <thead>
          <tr>
            <th className={thCls}>Role</th>
            <th className={thCls}>Base (bps)</th>
            <th className={thCls}>Effective (bps)</th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <td className={tdCls}>Maker</td>
            <td className={tdCls + ' font-mono'}>{f.makerBps}</td>
            <td className={tdCls + ' font-mono'}>{f.effectiveMakerBps}</td>
          </tr>
          <tr>
            <td className={tdCls}>Taker</td>
            <td className={tdCls + ' font-mono'}>{f.takerBps}</td>
            <td className={tdCls + ' font-mono'}>{f.effectiveTakerBps}</td>
          </tr>
        </tbody>
      </table>
      {f.promoActive && f.promoUntil ? (
        <p className="mt-2 text-xs text-amber-400">
          Promo window until {f.promoUntil}
          {f.promoMakerBps ? ` (maker ${f.promoMakerBps}bps` : ''}
          {f.promoTakerBps ? `, taker ${f.promoTakerBps}bps` : ''}
          {f.promoMakerBps ? ')' : ''}
        </p>
      ) : null}
    </section>
  );
}

// ---------------------------------------------------------------------------
// Solvency / proof-of-reserves
// ---------------------------------------------------------------------------

export function SolvencyPanel() {
  const q = useQuery({
    queryKey: ['reports', 'por-root'],
    queryFn: () => fetchProofRoot(apiClient),
    retry: (n_, e) => !(e instanceof ApiError && e.status === 501) && n_ < 2,
  });
  if (q.isPending) return <p className="text-sm text-neutral-500">Loading proof of reserves…</p>;
  if (q.isError) {
    if (isNotImplemented(q.error)) {
      return (
        <UnavailablePanel
          feature="Proof of reserves"
          owner="Phase-13 Task 13.3.7"
          note="The Merkle daily root and account solvency proofs arrive with the solvency scheduler. No reserve figures are displayed because none exist server-side."
        />
      );
    }
    return <ErrorBox error={q.error} />;
  }
  const p = q.data;
  return (
    <section aria-label="Solvency" className={cardCls}>
      <h2 className="text-base font-semibold text-neutral-100">Proof of reserves</h2>
      {p === null ? (
        <p className="mt-1 text-sm text-neutral-500">No published root.</p>
      ) : (
        <dl className="mt-2 space-y-1 text-sm">
          <div className="flex justify-between">
            <dt className="text-neutral-500">Daily Merkle root</dt>
            <dd className="font-mono text-neutral-200">{p.root ?? '—'}</dd>
          </div>
          <div className="flex justify-between">
            <dt className="text-neutral-500">As of</dt>
            <dd className="text-neutral-200">{p.date ?? '—'}</dd>
          </div>
          <div className="flex justify-between">
            <dt className="text-neutral-500">Total liabilities</dt>
            <dd className="font-mono text-neutral-200">{p.totalLiabilities ?? '—'}</dd>
          </div>
        </dl>
      )}
    </section>
  );
}

// ---------------------------------------------------------------------------
// System status
// ---------------------------------------------------------------------------

const STATE_STYLE: Record<string, string> = {
  operational: 'text-emerald-400',
  degraded: 'text-amber-400',
  degraded_performance: 'text-amber-400',
  partial_outage: 'text-orange-400',
  down: 'text-red-400',
  major_outage: 'text-red-400',
  maintenance: 'text-sky-400',
};

export function SystemInfoPanel() {
  const q = useQuery({
    queryKey: ['reports', 'system-status'],
    queryFn: () => fetchSystemStatus(apiClient),
    refetchInterval: 30_000,
    retry: (n_, e) => !(e instanceof ApiError && e.status === 501) && n_ < 2,
  });
  if (q.isPending) return <p className="text-sm text-neutral-500">Loading system status…</p>;
  if (q.isError) {
    if (isNotImplemented(q.error)) {
      return <UnavailablePanel feature="System status" owner="Phase-09 Task 9.3.18/9.3.25" />;
    }
    return <ErrorBox error={q.error} />;
  }
  const st = q.data;
  return (
    <section aria-label="System status" className={cardCls}>
      <div className="flex items-baseline justify-between">
        <h2 className="text-base font-semibold text-neutral-100">System status</h2>
        <span className={`text-sm font-medium ${STATE_STYLE[st.status] ?? 'text-neutral-400'}`}>
          {st.status}
        </span>
      </div>
      <p className="mt-1 text-xs text-neutral-500">
        Degradation mode: <span className="font-mono">{st.mode}</span> · source {st.source}
        {st.updatedAt ? ` · updated ${st.updatedAt}` : ''}
      </p>
      {st.components.length === 0 ? (
        <p className="mt-2 text-sm text-neutral-500">No component probes reported.</p>
      ) : (
        <table className={tableCls + ' mt-3'}>
          <thead>
            <tr>
              <th className={thCls}>Component</th>
              <th className={thCls}>State</th>
              <th className={thCls}>Latency</th>
              <th className={thCls}>Detail</th>
            </tr>
          </thead>
          <tbody>
            {st.components.map((c) => (
              <tr key={c.name}>
                <td className={tdCls}>
                  {c.name}
                  {c.critical ? ' (critical)' : ''}
                </td>
                <td className={`${tdCls} ${STATE_STYLE[c.state] ?? ''}`}>{c.state}</td>
                <td className={tdCls + ' font-mono'}>
                  {c.latencyMs !== null ? `${c.latencyMs.toFixed(1)}ms` : '—'}
                </td>
                <td className={tdCls}>{c.detail ?? '—'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {Object.keys(st.metrics).length > 0 ? (
        <dl className="mt-3 grid grid-cols-2 gap-1 text-xs md:grid-cols-4">
          {Object.entries(st.metrics).map(([k, v]) => (
            <div key={k}>
              <dt className="text-neutral-500">{k}</dt>
              <dd className="font-mono text-neutral-300">{v}</dd>
            </div>
          ))}
        </dl>
      ) : null}
    </section>
  );
}

// ---------------------------------------------------------------------------
// Announcements (dismissible, severity/category graded)
// ---------------------------------------------------------------------------

const DISMISS_KEY = 'exc.dismissed-announcements.v1';

function loadDismissed(): ReadonlySet<string> {
  try {
    const raw = window.localStorage.getItem(DISMISS_KEY);
    const arr: unknown = raw === null ? [] : JSON.parse(raw);
    return new Set(Array.isArray(arr) ? arr.filter((x): x is string => typeof x === 'string') : []);
  } catch {
    return new Set();
  }
}

const ANNOUNCEMENT_CATEGORY_STYLE: Record<string, string> = {
  INCIDENT: 'bg-red-500/20 text-red-400',
  MAINTENANCE: 'bg-sky-500/20 text-sky-400',
  PRODUCT: 'bg-emerald-500/20 text-emerald-400',
  PROMOTION: 'bg-violet-500/20 text-violet-400',
  GENERAL: 'bg-neutral-700/40 text-neutral-300',
};

export function AnnouncementsPanel() {
  const q = useQuery({
    queryKey: ['reports', 'announcements'],
    queryFn: () => fetchAnnouncements({}, apiClient),
    refetchInterval: 60_000,
  });
  const [dismissed, setDismissed] = useState<ReadonlySet<string>>(() => loadDismissed());

  const dismiss = (id: string) => {
    const next = new Set(dismissed);
    next.add(id);
    setDismissed(next);
    try {
      window.localStorage.setItem(DISMISS_KEY, JSON.stringify([...next]));
    } catch {
      // storage blocked — dismissal remains in-memory for the session
    }
  };

  if (q.isPending) return <p className="text-sm text-neutral-500">Loading announcements…</p>;
  if (q.isError) return <ErrorBox error={q.error} />;
  const visible = q.data.filter((a) => !dismissed.has(a.id));
  return (
    <section aria-label="Announcements" className="space-y-2">
      {visible.length === 0 ? (
        <p className="text-sm text-neutral-500">No active announcements.</p>
      ) : (
        visible.map((a: Announcement) => (
          <article key={a.id} className={cardCls + ' flex items-start gap-3'}>
            <span
              className={`mt-0.5 rounded px-2 py-0.5 text-xs font-semibold ${ANNOUNCEMENT_CATEGORY_STYLE[a.category] ?? ANNOUNCEMENT_CATEGORY_STYLE['GENERAL']}`}
            >
              {a.category}
            </span>
            <div className="min-w-0 flex-1">
              <h3 className="text-sm font-semibold text-neutral-100">{a.title}</h3>
              <p className="mt-0.5 whitespace-pre-wrap text-sm text-neutral-400">{a.body}</p>
              {a.publishAt ? (
                <p className="mt-1 text-xs text-neutral-600">Published {a.publishAt}</p>
              ) : null}
            </div>
            <button
              type="button"
              aria-label={`Dismiss announcement ${a.title}`}
              className="text-neutral-500 hover:text-neutral-200"
              onClick={() => {
                dismiss(a.id);
              }}
            >
              ✕
            </button>
          </article>
        ))
      )}
    </section>
  );
}
