/**
 * Conduct, governance & DORA console (Phase-10.5 Task 10.5.3.15 —
 * Batch E close) — regulatory-change management, execution policies,
 * product profiles & target markets, FX Global Code assessments,
 * governance packs, recertification, data residency, and the DORA
 * ICT provider register. Route: /admin/conduct.
 */
import { apiClient } from '@/app/runtime';
import { EnvPill, EnvSwitcher, EnvWatermark, useBoundAdminApi } from '@/lib/env';

import { RequireAdmin } from '../admin/RequireAdmin';
import { useAdminRole } from '../admin/adminRole';
import { DoraPanel } from './DoraPanel';
import { FXGCPanel } from './FXGCPanel';
import { PoliciesPanel } from './PoliciesPanel';
import { RegChangesPanel } from './RegChangesPanel';

export default function ConductPage() {
  return (
    <RequireAdmin>
      <ConductDashboard />
    </RequireAdmin>
  );
}

function ConductDashboard() {
  const adminApi = useBoundAdminApi(apiClient);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Conduct &amp; governance</h1>
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
        <RegChangesPanel adminApi={adminApi} />
        <PoliciesPanel adminApi={adminApi} />
        <FXGCPanel adminApi={adminApi} />
        <DoraPanel adminApi={adminApi} />
      </div>
    </div>
  );
}
