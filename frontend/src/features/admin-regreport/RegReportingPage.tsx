/**
 * Regulatory reporting desk (Phase-10.5 Task 10.5.3.13 — Batch E
 * start) — submissions lifecycle (repair queue, break dispositions,
 * corrected resubmission, ACK/NACK ingest, party identifiers,
 * reconcile), RTS 27/28 best-execution production, and the canned
 * regime reports (EMIR events, Basel III, compliance export).
 * Route: /admin/regreporting.
 */
import { apiClient } from '@/app/runtime';
import { EnvPill, EnvSwitcher, EnvWatermark, useBoundAdminApi } from '@/lib/env';

import { RequireAdmin } from '../admin/RequireAdmin';
import { useAdminRole } from '../admin/adminRole';
import { BestExecPanel } from './BestExecPanel';
import { RegimePanel } from './RegimePanel';
import { SubmissionsPanel } from './SubmissionsPanel';

export default function RegReportingPage() {
  return (
    <RequireAdmin>
      <RegReportingDashboard />
    </RequireAdmin>
  );
}

function RegReportingDashboard() {
  const adminApi = useBoundAdminApi(apiClient);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Regulatory reporting</h1>
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
        <SubmissionsPanel adminApi={adminApi} />
        <BestExecPanel adminApi={adminApi} />
        <RegimePanel adminApi={adminApi} />
      </div>
    </div>
  );
}
