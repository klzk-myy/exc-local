/**
 * Liquidity-provider detail panel — extends ConsolesPanel's LP
 * inventory + scorecard with the per-LP detail view (instrument feed
 * configs), guarded lifecycle update (PUT), and the performance-alert
 * trail (?open=1).
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import { apiClient } from '@/app/runtime';
import type { BoundAdminApi } from '@/lib/env';
import {
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
import { fetchLpAlerts, fetchLpDetail, updateLp } from './api';

const LP_STATUSES = ['ONBOARDING', 'ACTIVE', 'SUSPENDED'];

export function LpPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [lpId, setLpId] = useState('');
  const [openOnly, setOpenOnly] = useState(true);
  const [update, setUpdate] = useState({ status: 'SUSPENDED', staleness: '', reason: '' });
  const [notice, setNotice] = useState<string | null>(null);

  const id = Number(lpId);
  const detail = useQuery({
    queryKey: ['admin', 'lp-detail', adminApi.env, id],
    queryFn: () => fetchLpDetail(adminApi, id),
    enabled: id > 0,
    retry: false,
  });
  const alerts = useQuery({
    queryKey: ['admin', 'lp-alerts', adminApi.env, id, openOnly],
    queryFn: () => fetchLpAlerts(adminApi, id, openOnly),
    enabled: id > 0,
    retry: false,
  });

  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : String(e));
  const updateMut = useMutation({
    mutationFn: () =>
      updateLp(apiClient, adminApi.env, id, {
        status: update.status,
        ...(update.staleness !== '' ? { staleness_timeout_ms: Number(update.staleness) } : {}),
        reason: update.reason,
      }),
    onSuccess: () => {
      setNotice(`LP #${id} updated — guarded lifecycle enforced server-side.`);
      void qc.invalidateQueries({ queryKey: ['admin', 'lp-detail'] });
    },
    onError: onErr,
  });

  const denied = isAccessDenied(detail.error ?? alerts.error);
  if (denied) return <AccessDeniedCard detail="LP management requires a Risk Manager role." />;

  return (
    <section className={cardCls} aria-label="Liquidity providers">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">
        LP detail &amp; performance alerts
      </h2>
      <ErrorBox error={detail.error ?? alerts.error} />
      {notice !== null && (
        <p className="mb-2 rounded border border-neutral-700 bg-neutral-950 px-2 py-1 text-xs text-neutral-300">
          {notice}
        </p>
      )}
      <div className="mb-2 flex items-center gap-2">
        <label className="block">
          <span className="sr-only">LP id</span>
          <input
            aria-label="LP id"
            className={inputCls}
            placeholder="lp_id"
            value={lpId}
            onChange={(e) => setLpId(e.target.value)}
          />
        </label>
        <label className="flex items-center gap-1 text-xs text-neutral-300">
          <input
            type="checkbox"
            aria-label="Open alerts only"
            checked={openOnly}
            onChange={(e) => setOpenOnly(e.target.checked)}
          />
          open alerts only
        </label>
      </div>
      {detail.data && (
        <>
          <div className="mb-2 flex flex-wrap items-center gap-3 text-xs text-neutral-400">
            <span className="text-sm text-neutral-200">{detail.data.name}</span>
            <StatusBadge value={detail.data.status} />
            <span>{detail.data.connectionType}</span>
            <span>FIX {detail.data.fixSessionEnabled ? 'enabled' : 'off'}</span>
            <span>staleness {detail.data.stalenessTimeoutMs}ms</span>
          </div>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Instrument</th>
                <th className={thCls}>Enabled</th>
                <th className={thCls}>Markup bps (bid/ask)</th>
                <th className={thCls}>Skew bps</th>
                <th className={thCls}>Staleness ms</th>
              </tr>
            </thead>
            <tbody>
              {detail.data.instruments.map((i) => (
                <tr key={i.instrumentId}>
                  <td className={tdCls}>{i.symbol !== '' ? i.symbol : `#${i.instrumentId}`}</td>
                  <td className={tdCls}>{i.enabled ? 'yes' : 'no'}</td>
                  <td className={tdCls}>
                    {i.spreadMarkupBidBps} / {i.spreadMarkupAskBps}
                  </td>
                  <td className={tdCls}>{i.skewBps}</td>
                  <td className={tdCls}>{i.stalenessTimeoutMs}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <form
            className="mt-2 grid gap-2 sm:grid-cols-4"
            onSubmit={(e) => {
              e.preventDefault();
              updateMut.mutate();
            }}
          >
            <label className="block">
              <span className="sr-only">LP status</span>
              <select
                aria-label="LP status"
                className={selectCls}
                value={update.status}
                onChange={(e) => setUpdate({ ...update, status: e.target.value })}
              >
                {LP_STATUSES.map((s) => (
                  <option key={s} value={s}>
                    {s}
                  </option>
                ))}
              </select>
            </label>
            <label className="block">
              <span className="sr-only">staleness_timeout_ms</span>
              <input
                aria-label="staleness_timeout_ms"
                className={inputCls}
                placeholder="staleness_timeout_ms (opt)"
                value={update.staleness}
                onChange={(e) => setUpdate({ ...update, staleness: e.target.value })}
              />
            </label>
            <label className="block">
              <span className="sr-only">Update reason</span>
              <input
                aria-label="Update reason"
                className={inputCls}
                placeholder="reason (audit trail)"
                value={update.reason}
                onChange={(e) => setUpdate({ ...update, reason: e.target.value })}
              />
            </label>
            <button
              type="submit"
              className={btnPrimary}
              disabled={updateMut.isPending || update.reason === ''}
            >
              Update LP
            </button>
          </form>
          <p className={hintTextCls}>
            Lifecycle transitions are guarded server-side (ONBOARDING→ACTIVE→SUSPENDED); illegal
            moves are refused.
          </p>
        </>
      )}
      {id > 0 && (
        <>
          <h3 className="mt-4 mb-1 text-xs font-medium text-neutral-400">Performance alerts</h3>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Metric</th>
                <th className={thCls}>Observed</th>
                <th className={thCls}>Threshold</th>
                <th className={thCls}>Window</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Emitted</th>
              </tr>
            </thead>
            <tbody>
              {(alerts.data ?? []).map((a) => (
                <tr key={a.id}>
                  <td className={tdCls}>{a.id}</td>
                  <td className={tdCls}>{a.metric}</td>
                  <td className={tdCls}>{a.observed}</td>
                  <td className={tdCls}>{a.threshold}</td>
                  <td className={tdCls}>{a.window}</td>
                  <td className={tdCls}>
                    <StatusBadge value={a.status} />
                  </td>
                  <td className={tdCls}>{a.emittedAt.slice(0, 16)}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {alerts.data?.length === 0 && (
            <p className="py-2 text-center text-xs text-neutral-500">No alerts.</p>
          )}
        </>
      )}
      <p className={hintTextCls}>
        The LP inventory and 1h scorecards live on the Ops Consoles panel — this surface owns the
        entity detail, guarded update, and alert trail.
      </p>
    </section>
  );
}
