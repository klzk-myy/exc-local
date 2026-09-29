/**
 * System health panel (Task 10.3.6) — renders the aggregate status feed
 * (`GET /api/v1/system/status`, public) plus the admin health export
 * (`GET /api/v1/admin/ops/health`, env-scoped). The gateway-local
 * fallback badge is surfaced verbatim — a `source=gateway-local` doc is
 * the truthful "aggregator unavailable" signal, never a fabricated
 * "operational".
 */
import { useQuery } from '@tanstack/react-query';

import type { ApiClient } from '@/lib/api';
import type { BoundAdminApi } from '@/lib/env';
import { fetchOpsHealth, fetchSystemStatus, type SystemStatus } from '@/lib/admin/api';
import { cardCls, ErrorBox, StatusBadge } from '@/lib/ui';
import { AccessDeniedCard } from './RequireAdmin';
import { isAccessDenied } from './adminRole';

const STATE_TONE: Record<string, string> = {
  operational: 'text-emerald-400',
  degraded: 'text-amber-400',
  degraded_performance: 'text-amber-400',
  partial_outage: 'text-amber-400',
  down: 'text-red-400',
  major_outage: 'text-red-400',
  maintenance: 'text-sky-400',
  unknown: 'text-neutral-400',
};

function StatusBody({ status }: { status: SystemStatus }) {
  return (
    <div>
      <div className="flex items-center gap-2">
        <span
          className={`text-lg font-semibold ${STATE_TONE[status.status] ?? 'text-neutral-300'}`}
          data-testid="system-status"
        >
          {status.status.replaceAll('_', ' ')}
        </span>
        <span className="rounded bg-neutral-700/40 px-2 py-0.5 text-xs text-neutral-300">
          mode: {status.mode}
        </span>
        {status.source === 'gateway-local' && (
          <span
            className="rounded bg-amber-500/20 px-2 py-0.5 text-xs text-amber-300"
            title={status.detail ?? 'status aggregator unavailable — local view'}
          >
            gateway-local fallback
          </span>
        )}
      </div>
      {status.components.length > 0 && (
        <ul className="mt-2 space-y-1">
          {status.components.map((c) => (
            <li key={c.name} className="flex items-center gap-2 text-xs">
              <span className={STATE_TONE[c.state] ?? 'text-neutral-400'}>{c.name}</span>
              <span className="text-neutral-500">
                {c.state} · {c.latencyMs.toFixed(1)}ms{c.critical ? ' · critical' : ''}
              </span>
            </li>
          ))}
        </ul>
      )}
      <p className="mt-2 text-xs text-neutral-500">updated {status.updatedAt || '—'}</p>
    </div>
  );
}

export function HealthPanel({
  adminApi,
  systemApi,
}: {
  adminApi: BoundAdminApi;
  systemApi: ApiClient;
}) {
  const status = useQuery({
    queryKey: ['admin', 'system-status'],
    queryFn: () => fetchSystemStatus(systemApi),
    refetchInterval: 15_000,
    retry: false,
  });
  const health = useQuery({
    queryKey: ['admin', 'ops-health', adminApi.env],
    queryFn: () => fetchOpsHealth(adminApi),
    refetchInterval: 15_000,
    retry: false,
  });

  const denied =
    (status.error !== null && isAccessDenied(status.error)) ||
    (health.error !== null && isAccessDenied(health.error));

  return (
    <section className={cardCls} aria-label="System health">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">System health</h2>
      {denied && <AccessDeniedCard detail="Health export requires an authorized admin role." />}
      {!denied && <ErrorBox error={status.error ?? health.error} />}
      {!denied && status.data !== undefined && <StatusBody status={status.data} />}
      {!denied && status.data === undefined && status.error === null && (
        <p className="text-sm text-neutral-500">Loading status…</p>
      )}
      {!denied && health.data !== undefined && (
        <div className="mt-3 border-t border-neutral-800 pt-2 text-xs text-neutral-500">
          <p>
            Admin export ({adminApi.env}): {Object.keys(health.data.components).length} component
            record(s), {health.data.recentEvents.length} recent event(s)
            {health.data.modeReason !== undefined && ` · mode reason: ${health.data.modeReason}`}
          </p>
        </div>
      )}
      {health.data !== undefined && Object.keys(health.data.components).length > 0 && (
        <ul className="mt-1 space-y-1">
          {Object.entries(health.data.components).map(([name, fields]) => (
            <li key={name} className="flex items-center gap-2 text-xs">
              <StatusBadge value={(fields['state'] ?? 'unknown').toUpperCase()} />
              <span className="text-neutral-400">{name}</span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
