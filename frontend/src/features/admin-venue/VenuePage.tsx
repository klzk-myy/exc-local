/**
 * Regulated-venue governance console (Phase-10.5 Task 10.5.3.14 —
 * Batch E) — the Phase-21 Task 21.3.15 surface: member admission &
 * lifecycle, rulebook versions + participant notices, market-control
 * interventions, investigation/disciplinary cases, conflicts of
 * interest, self-assessments, CCO reports, and the launch gate.
 * Route: /admin/venue.
 */
import { apiClient } from '@/app/runtime';
import { EnvPill, EnvSwitcher, EnvWatermark, useBoundAdminApi } from '@/lib/env';

import { RequireAdmin } from '../admin/RequireAdmin';
import { useAdminRole } from '../admin/adminRole';
import { AssurancePanel } from './AssurancePanel';
import { MembersPanel } from './MembersPanel';
import { OversightPanel } from './OversightPanel';
import { RulebooksPanel } from './RulebooksPanel';

export default function VenuePage() {
  return (
    <RequireAdmin>
      <VenueDashboard />
    </RequireAdmin>
  );
}

function VenueDashboard() {
  const adminApi = useBoundAdminApi(apiClient);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Venue governance</h1>
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
        <MembersPanel adminApi={adminApi} />
        <RulebooksPanel adminApi={adminApi} />
        <OversightPanel adminApi={adminApi} />
        <AssurancePanel adminApi={adminApi} />
      </div>
    </div>
  );
}
