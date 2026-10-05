/**
 * Instrument lifecycle console (Phase-10.5 Task 10.5.3.11 — Batch D
 * start) — the governance surfaces around the instrument lifecycle:
 * listing proposals (intake + review incl. four-eyes APPROVE), the
 * per-symbol auction calendar (full-replace under dual control), the
 * 24/5 market schedule with holiday overrides, and the
 * dual-controlled margin-parameter / entity-leverage policy cells.
 *
 * The instrument list + state-machine verbs themselves live on the
 * Admin page (`features/admin/InstrumentsPanel`, Task 15.3.2) — this
 * console covers the plan's remaining surfaces without duplicating it.
 * Route: /admin/instrument-governance.
 */
import { apiClient } from '@/app/runtime';
import { EnvPill, EnvSwitcher, EnvWatermark, useBoundAdminApi } from '@/lib/env';

import { RequireAdmin } from '../admin/RequireAdmin';
import { useAdminRole } from '../admin/adminRole';
import { ListingPanel } from './ListingPanel';
import { RiskPolicyPanel } from './RiskPolicyPanel';
import { SchedulePanel } from './SchedulePanel';

export default function InstrumentGovernancePage() {
  return (
    <RequireAdmin>
      <GovernanceDashboard />
    </RequireAdmin>
  );
}

function GovernanceDashboard() {
  const adminApi = useBoundAdminApi(apiClient);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Instrument governance</h1>
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
        <ListingPanel adminApi={adminApi} />
        <SchedulePanel adminApi={adminApi} />
        <RiskPolicyPanel adminApi={adminApi} />
      </div>
    </div>
  );
}
