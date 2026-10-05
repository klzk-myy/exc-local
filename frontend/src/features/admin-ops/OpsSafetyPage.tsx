/**
 * Ops-safety console (Phase-10.5 Task 10.5.3.1) — the fail-safe levers
 * over already-mounted backends: kill switch (Phase-11), circuit
 * breaker (Phase-13), feature flags (Task 9.3.7), IP bans/allowlist
 * (Task 5.3.34), maintenance windows (Task 5.3.14), and the destructive
 * trade-ops endpoints (mass cancel, manual liquidation, cache warm,
 * FIX-session entitlement).
 *
 * Deliberately a separate page from /admin (the landing dashboard keeps
 * its Task 10.3.6 shape) and from /ops (fleet/release surfaces keep
 * their Task 10.3.20 owners) — no competing surfaces.
 */
import { apiClient } from '@/app/runtime';
import { EnvPill, EnvSwitcher, EnvWatermark, useBoundAdminApi } from '@/lib/env';

import { RequireAdmin } from '../admin/RequireAdmin';
import { useAdminRole } from '../admin/adminRole';
import { CircuitBreakerPanel } from './CircuitBreakerPanel';
import { DestructiveOpsPanel } from './DestructiveOpsPanel';
import { FlagsPanel } from './FlagsPanel';
import { IpSecurityPanel } from './IpSecurityPanel';
import { KillSwitchPanel } from './KillSwitchPanel';
import { MaintenancePanel } from './MaintenancePanel';
import { SecurityOpsPanel } from './SecurityOpsPanel';
import { TradeOpsPanel } from './TradeOpsPanel';

export default function OpsSafetyPage() {
  return (
    <RequireAdmin>
      <OpsSafetyDashboard />
    </RequireAdmin>
  );
}

function OpsSafetyDashboard() {
  const adminApi = useBoundAdminApi(apiClient);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Ops safety</h1>
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
        <KillSwitchPanel adminApi={adminApi} />
        <CircuitBreakerPanel adminApi={adminApi} />
        <FlagsPanel adminApi={adminApi} />
        <IpSecurityPanel adminApi={adminApi} />
        <MaintenancePanel adminApi={adminApi} />
        <TradeOpsPanel adminApi={adminApi} />
        <SecurityOpsPanel adminApi={adminApi} />
        <DestructiveOpsPanel adminApi={adminApi} />
      </div>
    </div>
  );
}
