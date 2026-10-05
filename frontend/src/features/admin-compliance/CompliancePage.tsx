/**
 * Compliance ops console (Phase-10.5 Task 10.5.3.5) — day-to-day
 * client compliance actions: on-demand screening + adverse-media
 * intake, travel-rule MISSING_INFO cure, restricted-list
 * administration, employee-dealing pre-clearance + audit, and the
 * WARN→SUSPEND enforcement ladder. Route: /admin/compliance.
 */
import { apiClient } from '@/app/runtime';
import { EnvPill, EnvSwitcher, EnvWatermark, useBoundAdminApi } from '@/lib/env';

import { RequireAdmin } from '../admin/RequireAdmin';
import { useAdminRole } from '../admin/adminRole';
import { EmployeeDealingPanel } from './EmployeeDealingPanel';
import { EnforcementPanel } from './EnforcementPanel';
import { RestrictedListsPanel } from './RestrictedListsPanel';
import { ScreeningPanel } from './ScreeningPanel';
import { TravelRulePanel } from './TravelRulePanel';

export default function CompliancePage() {
  return (
    <RequireAdmin>
      <ComplianceDashboard />
    </RequireAdmin>
  );
}

function ComplianceDashboard() {
  const adminApi = useBoundAdminApi(apiClient);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Compliance ops</h1>
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
        <ScreeningPanel adminApi={adminApi} />
        <TravelRulePanel adminApi={adminApi} />
        <RestrictedListsPanel adminApi={adminApi} />
        <EmployeeDealingPanel adminApi={adminApi} />
        <EnforcementPanel adminApi={adminApi} />
      </div>
    </div>
  );
}
