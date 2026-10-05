/**
 * API-deprecation panel (Phase-10.5 Task 10.5.3.3 §5) — the Task 5.3.20
 * policy surface: announce rules (sunset ≥ announced + 6 months,
 * server-enforced), list them, and render per-rule usage telemetry
 * (Redis hit counters rolled up per day).
 */
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnPrimary,
  cardCls,
  ErrorBox,
  hintTextCls,
  inputCls,
  labelCls,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { AccessDeniedCard } from '../admin/RequireAdmin';
import { isAccessDenied } from '../admin/adminRole';
import { announceDeprecation, fetchDeprecationUsage } from './api';

const toRfc3339 = (local: string): string => {
  if (local === '') return '';
  const d = new Date(local);
  return Number.isNaN(d.getTime()) ? local : d.toISOString();
};

export function DeprecationPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [path, setPath] = useState('');
  const [method, setMethod] = useState('');
  const [sunset, setSunset] = useState('');
  const [replacement, setReplacement] = useState('');
  const [notice, setNotice] = useState('');
  const [matchPrefix, setMatchPrefix] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [created, setCreated] = useState<string | null>(null);

  const query = useQuery({
    queryKey: ['admin-int', 'deprecation-usage', adminApi.env],
    queryFn: () => fetchDeprecationUsage(adminApi),
    retry: false,
    refetchInterval: 60_000,
  });

  if (isAccessDenied(query.error)) {
    return <AccessDeniedCard detail="Deprecation telemetry requires an auditor-capable role." />;
  }

  const submit = async () => {
    setBusy(true);
    setError(null);
    setCreated(null);
    try {
      await announceDeprecation(adminApi, {
        path: path.trim(),
        method: method.trim() === '' ? undefined : method.trim().toUpperCase(),
        matchPrefix,
        sunsetAt: toRfc3339(sunset),
        replacement: replacement.trim() === '' ? undefined : replacement.trim(),
        notice: notice.trim() === '' ? undefined : notice.trim(),
      });
      setCreated(`Deprecation announced for ${path.trim()}`);
      setPath('');
      setSunset('');
      setReplacement('');
      setNotice('');
      await qc.invalidateQueries({ queryKey: ['admin-int', 'deprecation-usage'] });
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className={cardCls} aria-label="API deprecations">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">API deprecations</h2>
      <p className={hintTextCls}>
        Announced endpoints emit Deprecation/Sunset headers; past sunset they answer 410
        ENDPOINT_GONE. Policy requires ≥6 months notice (server CHECK).
      </p>

      {query.isError && <ErrorBox error={query.error} />}
      {error !== null && <ErrorBox error={error} />}
      {created !== null && (
        <p className="mb-2 text-sm text-emerald-300" role="status">
          {created}
        </p>
      )}

      <div className="mb-3 grid gap-2 sm:grid-cols-2 lg:grid-cols-6">
        <div className="lg:col-span-2">
          <label className={labelCls} htmlFor="dep-path">
            Path
          </label>
          <input
            id="dep-path"
            className={inputCls}
            value={path}
            placeholder="/api/v1/orders/legacy"
            onChange={(e) => setPath(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="dep-method">
            Method (empty = all)
          </label>
          <input
            id="dep-method"
            className={inputCls}
            value={method}
            placeholder="GET"
            onChange={(e) => setMethod(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="dep-sunset">
            Sunset (≥6 months out)
          </label>
          <input
            id="dep-sunset"
            type="datetime-local"
            className={inputCls}
            value={sunset}
            onChange={(e) => setSunset(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="dep-repl">
            Replacement
          </label>
          <input
            id="dep-repl"
            className={inputCls}
            value={replacement}
            placeholder="/api/v2/orders"
            onChange={(e) => setReplacement(e.target.value)}
          />
        </div>
        <div className="flex items-end gap-2">
          <label className="flex items-center gap-2 text-sm text-neutral-300">
            <input
              type="checkbox"
              className="h-4 w-4"
              checked={matchPrefix}
              onChange={(e) => setMatchPrefix(e.target.checked)}
            />
            Prefix match
          </label>
          <button
            type="button"
            className={btnPrimary}
            disabled={busy || path.trim() === '' || sunset === ''}
            onClick={() => void submit()}
          >
            Announce
          </button>
        </div>
      </div>
      <div className="mb-3">
        <label className={labelCls} htmlFor="dep-notice">
          Notice (optional)
        </label>
        <input
          id="dep-notice"
          className={inputCls}
          value={notice}
          onChange={(e) => setNotice(e.target.value)}
        />
      </div>

      {query.isSuccess &&
        (query.data.length === 0 ? (
          <p className="text-sm text-neutral-500">No deprecation rules announced.</p>
        ) : (
          <div className="max-h-64 overflow-y-auto">
            <table className={tableCls}>
              <thead>
                <tr>
                  <th className={thCls}>Method</th>
                  <th className={thCls}>Path</th>
                  <th className={thCls}>Sunset</th>
                  <th className={thCls}>Replacement</th>
                  <th className={thCls}>Recent hits</th>
                </tr>
              </thead>
              <tbody>
                {query.data.map((u) => {
                  const days = Object.entries(u.daily).sort().slice(-7);
                  const hits = days.reduce((s, [, pair]) => s + pair[0], 0);
                  return (
                    <tr key={u.id}>
                      <td className={tdCls}>{u.method ?? '*'}</td>
                      <td className={tdCls}>
                        <code className="text-xs">
                          {u.path}
                          {u.matchPrefix ? '*' : ''}
                        </code>
                        {u.notice !== undefined && (
                          <div className="text-xs text-neutral-500">{u.notice}</div>
                        )}
                      </td>
                      <td className={tdCls}>{u.sunsetAt.slice(0, 10)}</td>
                      <td className={tdCls}>
                        {u.replacement !== undefined ? (
                          <code className="text-xs">{u.replacement}</code>
                        ) : (
                          '—'
                        )}
                      </td>
                      <td className={tdCls} title={JSON.stringify(u.daily)}>
                        {hits > 0 ? `${hits} (7d)` : '0'}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        ))}
    </section>
  );
}
