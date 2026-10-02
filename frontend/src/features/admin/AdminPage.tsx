/**
 * Admin dashboard (Phase-10 Task 10.3.6) — the backoffice landing
 * surface: user lookup, support-queue counters, system health, audit
 * log, dual-control queue, and the live instrument-management panel
 * (Task 10.3.5 over the Phase-15 Task 15.3.2 lifecycle API).
 *
 * `/api/v1/admin/ops-board` is live (Phase-15 Task 15.3.12 — supersedes
 * the earlier registered-stub deferral note); the ops feature renders it
 * plus the live health surface.
 */
import { apiClient } from '@/app/runtime';
import { useBoundAdminApi, EnvSwitcher, EnvWatermark, EnvPill } from '@/lib/env';

import { RequireAdmin } from './RequireAdmin';
import { useAdminRole } from './adminRole';
import { HealthPanel } from './HealthPanel';
import { AuditLogPanel } from './AuditLogPanel';
import { SupportQueuePanel } from './SupportQueuePanel';
import { UserLookupPanel } from './UserLookupPanel';
import { DualControlPanel } from './DualControlPanel';
import { InstrumentsPanel } from './InstrumentsPanel';
import { ConsolesPanel } from './ConsolesPanel';

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
          <InstrumentsPanel adminApi={adminApi} api={apiClient} />
        </div>
        <div className="lg:col-span-2">
          <ConsolesPanel adminApi={adminApi} />
        </div>
      </div>
    </div>
  );
}
