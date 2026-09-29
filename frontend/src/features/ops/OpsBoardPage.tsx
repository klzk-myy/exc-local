/**
 * Ops board (Phase-10 Task 10.3.20 items 3–4) — component aggregation
 * from the live surfaces:
 *
 *   GET /api/v1/system/status       public aggregate (components[])
 *   GET /api/v1/admin/ops/health    admin export (env-scoped; auditor+)
 *
 * Registered-but-stub siblings render as unavailable panels instead of
 * fabricated content:
 *   GET /api/v1/admin/ops-board     Phase-15 Task 15.3.12 (market-ops
 *                                 console board — Risk Manager)
 *   /api/v1/admin/instruments/{id}/{activate|suspend|…}  Phase-15
 *                                 Task 15.3.1 lifecycle actions
 *
 * The task text references `/api/v1/admin/ops/status`; the route table
 * registers `ops/health` + `ops-board` — the live endpoints are used and
 * the drift is noted here (spec-truthful naming follows the registry).
 */
import { useQuery } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import type { ApiClient } from '@/lib/api';
import { fetchOpsHealth, fetchSystemStatus } from '@/lib/admin/api';
import { EnvSwitcher, EnvWatermark, EnvPill, useBoundAdminApi } from '@/lib/env';
import { cardCls, tableCls, tdCls, thCls, ErrorBox, StatusBadge } from '@/lib/ui';
import { AccessDeniedCard, RequireAdmin } from '@/features/admin/RequireAdmin';
import { isAccessDenied, useAdminRole } from '@/features/admin/adminRole';

function ComponentsTable({
  adminApi,
  systemApi,
}: {
  adminApi: ReturnType<typeof useBoundAdminApi>;
  systemApi: ApiClient;
}) {
  const status = useQuery({
    queryKey: ['ops', 'system-status'],
    queryFn: () => fetchSystemStatus(systemApi),
    refetchInterval: 15_000,
    retry: false,
  });
  const health = useQuery({
    queryKey: ['ops', 'health', adminApi.env],
    queryFn: () => fetchOpsHealth(adminApi),
    refetchInterval: 15_000,
    retry: false,
  });

  return (
    <section className={cardCls} aria-label="Component aggregation">
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-sm font-medium text-neutral-400">Component status</h2>
        {status.data !== undefined && (
          <span className="text-xs text-neutral-500">
            source: {status.data.source}
            {status.data.source === 'gateway-local' && (
              <span className="ml-1 rounded bg-amber-500/20 px-2 py-0.5 text-amber-300">
                gateway-local fallback — aggregator unavailable
              </span>
            )}
          </span>
        )}
      </div>
      <ErrorBox error={status.error} />
      {health.error !== null && isAccessDenied(health.error) && (
        <AccessDeniedCard detail="Admin health export requires an authorized admin role for this env." />
      )}
      {health.error !== null && !isAccessDenied(health.error) && <ErrorBox error={health.error} />}

      {status.data !== undefined && (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>Component</th>
              <th className={thCls}>State</th>
              <th className={thCls}>Critical</th>
              <th className={thCls}>Latency</th>
              <th className={thCls}>Detail</th>
            </tr>
          </thead>
          <tbody>
            {status.data.components.map((c) => (
              <tr key={c.name}>
                <td className={tdCls}>{c.name}</td>
                <td className={tdCls}>
                  <StatusBadge value={c.state.toUpperCase()} />
                </td>
                <td className={tdCls}>{c.critical ? 'yes' : 'no'}</td>
                <td className={tdCls}>{c.latencyMs.toFixed(1)}ms</td>
                <td className={tdCls}>{c.detail ?? '—'}</td>
              </tr>
            ))}
            {status.data.components.length === 0 && (
              <tr>
                <td className={tdCls} colSpan={5}>
                  Aggregate status: {status.data.status} (mode {status.data.mode}) — no component
                  rows in this document.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      )}

      {health.data !== undefined && Object.keys(health.data.components).length > 0 && (
        <>
          <h3 className="mt-3 mb-1 text-xs font-medium text-neutral-400">
            Admin export — component hashes ({adminApi.env})
          </h3>
          <table className={tableCls}>
            <tbody>
              {Object.entries(health.data.components).map(([name, fields]) => (
                <tr key={name}>
                  <td className={tdCls}>{name}</td>
                  <td className={tdCls}>
                    <StatusBadge value={(fields['state'] ?? 'unknown').toUpperCase()} />
                  </td>
                  <td className={tdCls}>
                    <code className="text-xs">{JSON.stringify(fields)}</code>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
      {health.data !== undefined && health.data.recentEvents.length > 0 && (
        <p className="mt-2 text-xs text-neutral-500">
          {health.data.recentEvents.length} recent status transition(s) recorded.
        </p>
      )}
    </section>
  );
}

function StubPanels({ role }: { role: string | null }) {
  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <section className={cardCls} aria-label="Market ops board">
        <h2 className="mb-2 text-sm font-medium text-neutral-400">Market-ops board</h2>
        <p className="text-sm text-neutral-500">
          <code>GET /api/v1/admin/ops-board</code> is registered but its handler ships with Phase-15
          Task 15.3.12. Required role when live: <strong>Risk Manager</strong>
          {role !== 'Risk Manager' && role !== null ? ' (your claim does not carry it)' : ''}.
        </p>
      </section>
      <section className={cardCls} aria-label="Instrument lifecycle">
        <h2 className="mb-2 text-sm font-medium text-neutral-400">Instrument lifecycle</h2>
        <p className="text-sm text-neutral-500">
          List-pair / suspend / restrict / cancel-only / halt / resume / delist actions are
          registered under <code>/api/v1/admin/instruments/…</code> (Phase-15 Task 15.3.1) and are
          dual-controlled in production. They activate when the handlers land — surfacing
          placeholder controls would not be honest.
        </p>
      </section>
    </div>
  );
}

export default function OpsBoardPage({ systemApi = apiClient }: { systemApi?: ApiClient }) {
  return (
    <RequireAdmin>
      <OpsBoard systemApi={systemApi} />
    </RequireAdmin>
  );
}

function OpsBoard({ systemApi }: { systemApi: ApiClient }) {
  const adminApi = useBoundAdminApi(systemApi);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Ops board</h1>
        <div className="flex items-center gap-3">
          <EnvSwitcher />
          <EnvPill />
        </div>
      </div>
      <div className="space-y-4">
        <ComponentsTable adminApi={adminApi} systemApi={systemApi} />
        <StubPanels role={role} />
      </div>
    </div>
  );
}
