/**
 * Admin dashboard (Phase-10 Task 10.3.6) — the backoffice landing
 * surface: user lookup, support-queue counters, system health, audit
 * log, dual-control queue.
 *
 * Two honest deferrals rendered in place (per the fail-closed rule):
 *   - Instrument management: the routes are registered (Phase-15 Task
 *     15.3.8) but Status=Stub — handler lands with Phase-15.
 *   - `/api/v1/admin/ops-board`: registered stub (Phase-15 Task
 *     15.3.12); the ops feature renders the live health surface instead.
 */
import { apiClient } from '@/app/runtime';
import { useBoundAdminApi, EnvSwitcher, EnvWatermark, EnvPill } from '@/lib/env';
import { cardCls } from '@/lib/ui';

import { RequireAdmin } from './RequireAdmin';
import { useAdminRole } from './adminRole';
import { HealthPanel } from './HealthPanel';
import { AuditLogPanel } from './AuditLogPanel';
import { SupportQueuePanel } from './SupportQueuePanel';
import { UserLookupPanel } from './UserLookupPanel';
import { DualControlPanel } from './DualControlPanel';

function InstrumentsNotice() {
  return (
    <section className={cardCls} aria-label="Instrument management">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">Instrument management</h2>
      <p className="text-sm text-neutral-500">
        Instrument lifecycle actions (list / activate / suspend / restrict / delist) are registered
        at <code>/api/v1/admin/instruments*</code> but the handlers land with Phase-15 (Task
        15.3.1/15.3.8) — this panel activates when that surface goes live. Trader-visible reference
        data remains available under{' '}
        <a href="/discovery" className="text-sky-400 underline">
          Discovery
        </a>
        .
      </p>
    </section>
  );
}

export default function AdminPage() {
  return (
    <RequireAdmin>
      <AdminDashboard />
    </RequireAdmin>
  );
}

function AdminDashboard() {
  const adminApi = useBoundAdminApi(apiClient);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Admin console</h1>
        <div className="flex items-center gap-3">
          <EnvSwitcher />
          {role !== null && (
            <span
              className="rounded bg-sky-500/20 px-2 py-0.5 text-xs font-medium text-sky-300"
              data-testid="admin-role-badge"
              title="Client-side hint only — the server is the authorizer"
            >
              {role}
            </span>
          )}
          <EnvPill />
        </div>
      </div>
      <div className="grid gap-4 lg:grid-cols-2">
        <HealthPanel adminApi={adminApi} systemApi={apiClient} />
        <UserLookupPanel adminApi={adminApi} />
        <SupportQueuePanel adminApi={adminApi} />
        <DualControlPanel adminApi={adminApi} />
        <div className="lg:col-span-2">
          <AuditLogPanel adminApi={adminApi} />
        </div>
        <div className="lg:col-span-2">
          <InstrumentsNotice />
        </div>
      </div>
    </div>
  );
}
