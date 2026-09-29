/**
 * Strategy browser (Task 10.3.26 item 1) — provider leaderboard from
 * GET /api/v1/copy/strategies with card/table toggle, client-side sort
 * and risk-class filter. On 501 NOT_IMPLEMENTED the surface renders
 * UnavailablePanel — zero fabricated leaderboard rows.
 */
import { useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError } from '@/lib/api';
import { ErrorBox, btnGhost, inputCls, selectCls, tableCls, tdCls, thCls } from '@/lib/ui';
import { UnavailablePanel, isNotImplemented } from '@/lib/input-helpers';

import { listStrategies, type CopyStrategy } from './api';

type SortKey = 'alias' | 'return30d' | 'return90d' | 'maxDrawdown' | 'sharpe' | 'aum' | 'followers';

const COLUMNS: { key: SortKey; label: string; numeric: boolean }[] = [
  { key: 'alias', label: 'Provider', numeric: false },
  { key: 'return30d', label: '30d return', numeric: true },
  { key: 'return90d', label: '90d return', numeric: true },
  { key: 'maxDrawdown', label: 'Max drawdown', numeric: true },
  { key: 'sharpe', label: 'Sharpe', numeric: true },
  { key: 'aum', label: 'AUM', numeric: true },
  { key: 'followers', label: 'Followers', numeric: true },
];

const RISK_CLASSES = ['ALL', 'LOW', 'MEDIUM', 'HIGH'] as const;

function numOr(v: string | number | null): number | null {
  if (v === null) return null;
  const n = Number(v);
  return Number.isFinite(n) ? n : null;
}

export function StrategyBrowser({ onFollow }: { onFollow: (s: CopyStrategy) => void }) {
  const q = useQuery({
    queryKey: ['copy-grid', 'strategies'],
    queryFn: () => listStrategies(apiClient),
    retry: (n, e) => !(e instanceof ApiError && e.status === 501) && n < 2,
  });
  const [view, setView] = useState<'table' | 'cards'>('table');
  const [sortKey, setSortKey] = useState<SortKey>('return30d');
  const [sortDir, setSortDir] = useState<'asc' | 'desc'>('desc');
  const [riskFilter, setRiskFilter] = useState<string>('ALL');
  const [search, setSearch] = useState('');

  const rows = useMemo(() => {
    let list = q.data ?? [];
    if (riskFilter !== 'ALL') {
      list = list.filter((s) => (s.riskClass ?? '').toUpperCase() === riskFilter);
    }
    if (search.trim() !== '') {
      const t = search.trim().toLowerCase();
      list = list.filter((s) => s.alias.toLowerCase().includes(t));
    }
    const dir = sortDir === 'asc' ? 1 : -1;
    return [...list].sort((a, b) => {
      const va = a[sortKey];
      const vb = b[sortKey];
      if (typeof va === 'string' && typeof vb === 'string' && sortKey === 'alias') {
        return va.localeCompare(vb) * dir;
      }
      const na = numOr(va);
      const nb = numOr(vb);
      if (na === null && nb === null) return 0;
      if (na === null) return 1;
      if (nb === null) return -1;
      return (na - nb) * dir;
    });
  }, [q.data, riskFilter, search, sortKey, sortDir]);

  if (q.isPending) return <p className="text-sm text-neutral-500">Loading strategies…</p>;
  if (q.isError) {
    if (isNotImplemented(q.error)) {
      return (
        <UnavailablePanel
          feature="Copy trading"
          owner="Phase-14 Task 14.3.8"
          note="The strategy leaderboard endpoint is registered but not yet live. No strategy data is shown because none exists server-side."
        />
      );
    }
    return <ErrorBox error={q.error} />;
  }

  return (
    <section aria-label="Strategy browser" className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <input
          className={inputCls + ' max-w-56'}
          placeholder="Search providers"
          value={search}
          onChange={(e) => {
            setSearch(e.target.value);
          }}
          aria-label="Search providers"
        />
        <select
          className={selectCls + ' w-auto'}
          value={riskFilter}
          onChange={(e) => {
            setRiskFilter(e.target.value);
          }}
          aria-label="Risk class filter"
        >
          {RISK_CLASSES.map((r) => (
            <option key={r} value={r}>
              {r === 'ALL' ? 'All risk classes' : `${r} risk`}
            </option>
          ))}
        </select>
        <div className="ml-auto flex gap-1" role="group" aria-label="View mode">
          <button
            type="button"
            className={btnGhost}
            aria-pressed={view === 'table'}
            onClick={() => {
              setView('table');
            }}
          >
            Table
          </button>
          <button
            type="button"
            className={btnGhost}
            aria-pressed={view === 'cards'}
            onClick={() => {
              setView('cards');
            }}
          >
            Cards
          </button>
        </div>
      </div>

      {rows.length === 0 ? (
        <p className="text-sm text-neutral-500">No strategies match the current filters.</p>
      ) : view === 'table' ? (
        <div className="overflow-x-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                {COLUMNS.map((c) => (
                  <th key={c.key} className={thCls}>
                    <button
                      type="button"
                      className="hover:text-neutral-200"
                      onClick={() => {
                        if (sortKey === c.key) setSortDir((d) => (d === 'asc' ? 'desc' : 'asc'));
                        else {
                          setSortKey(c.key);
                          setSortDir(c.numeric ? 'desc' : 'asc');
                        }
                      }}
                    >
                      {c.label}
                      {sortKey === c.key ? (sortDir === 'asc' ? ' ↑' : ' ↓') : ''}
                    </button>
                  </th>
                ))}
                <th className={thCls}>Risk</th>
                <th className={thCls} />
              </tr>
            </thead>
            <tbody>
              {rows.map((s) => (
                <tr key={s.id}>
                  <td className={tdCls}>{s.alias}</td>
                  <td className={tdCls}>{s.return30d ?? '—'}%</td>
                  <td className={tdCls}>{s.return90d ?? '—'}%</td>
                  <td className={tdCls}>{s.maxDrawdown ?? '—'}%</td>
                  <td className={tdCls}>{s.sharpe ?? '—'}</td>
                  <td className={tdCls}>{s.aum ?? '—'}</td>
                  <td className={tdCls}>{s.followers ?? '—'}</td>
                  <td className={tdCls}>{s.riskClass ?? '—'}</td>
                  <td className={tdCls}>
                    <button
                      type="button"
                      className={btnGhost}
                      onClick={() => {
                        onFollow(s);
                      }}
                    >
                      Follow
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <ul className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
          {rows.map((s) => (
            <li key={s.id} className="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
              <div className="flex items-baseline justify-between">
                <h3 className="font-medium text-neutral-100">{s.alias}</h3>
                <span className="text-xs text-neutral-500">{s.riskClass ?? 'risk n/a'}</span>
              </div>
              <dl className="mt-2 grid grid-cols-2 gap-x-4 gap-y-1 text-sm">
                <dt className="text-neutral-500">30d return</dt>
                <dd className="text-right text-neutral-200">{s.return30d ?? '—'}%</dd>
                <dt className="text-neutral-500">Max drawdown</dt>
                <dd className="text-right text-neutral-200">{s.maxDrawdown ?? '—'}%</dd>
                <dt className="text-neutral-500">Sharpe</dt>
                <dd className="text-right text-neutral-200">{s.sharpe ?? '—'}</dd>
                <dt className="text-neutral-500">Followers</dt>
                <dd className="text-right text-neutral-200">{s.followers ?? '—'}</dd>
              </dl>
              <button
                type="button"
                className={btnGhost + ' mt-3 w-full'}
                onClick={() => {
                  onFollow(s);
                }}
              >
                Follow
              </button>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
