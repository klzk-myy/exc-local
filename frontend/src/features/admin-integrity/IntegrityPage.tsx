/**
 * Integrity console (Phase-10.5 Task 10.5.3.3) — the read/verify
 * surface for the immutable audit chain, reconciliation evidence,
 * order-record export, WAL archive, DLQ and API-deprecation telemetry.
 * Route: /admin/integrity. Read-mostly — the only mutation is the
 * deprecation announce (server-audited).
 */
import { apiClient } from '@/app/runtime';
import { EnvPill, EnvSwitcher, EnvWatermark, useBoundAdminApi } from '@/lib/env';

import { RequireAdmin } from '../admin/RequireAdmin';
import { useAdminRole } from '../admin/adminRole';
import { RecordsArchivePanel, DlqPanel } from './ArchiveDlqPanel';
import { AuditIntegrityPanel } from './AuditIntegrityPanel';
import { DeprecationPanel } from './DeprecationPanel';
import { ReconPanel } from './ReconPanel';

export default function IntegrityPage() {
  return (
    <RequireAdmin>
      <IntegrityDashboard />
    </RequireAdmin>
  );
}

function IntegrityDashboard() {
  const adminApi = useBoundAdminApi(apiClient);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Integrity</h1>
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
      <div className="grid gap-4">
        <AuditIntegrityPanel adminApi={adminApi} />
        <ReconPanel adminApi={adminApi} />
        <RecordsArchivePanel adminApi={adminApi} />
        <DlqPanel adminApi={adminApi} />
        <DeprecationPanel adminApi={adminApi} />
      </div>
    </div>
  );
}
