/**
 * Settlement & allocation ops console (Phase-10.5 Task 10.5.3.9) —
 * post-trade operations: CLS PvP instruction lifecycle, settlement
 * exceptions (dual-control resolutions), MT900/910 confirmation
 * intake, allocation groups + T+0 escalation, chargeback register.
 * Route: /admin/settlement.
 */
import { apiClient } from '@/app/runtime';
import { EnvPill, EnvSwitcher, EnvWatermark, useBoundAdminApi } from '@/lib/env';

import { RequireAdmin } from '../admin/RequireAdmin';
import { useAdminRole } from '../admin/adminRole';
import { AllocationsPanel } from './AllocationsPanel';
import { BackofficePanel } from './BackofficePanel';
import { ChargebacksPanel } from './ChargebacksPanel';
import { ClsPanel } from './ClsPanel';
import { ExceptionsPanel } from './ExceptionsPanel';
import { NettingPanel } from './NettingPanel';

export default function SettlementPage() {
  return (
    <RequireAdmin>
      <SettlementDashboard />
    </RequireAdmin>
  );
}

function SettlementDashboard() {
  const adminApi = useBoundAdminApi(apiClient);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Settlement &amp; allocations</h1>
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
        <ClsPanel adminApi={adminApi} />
        <NettingPanel adminApi={adminApi} />
        <BackofficePanel adminApi={adminApi} />
        <ExceptionsPanel adminApi={adminApi} />
        <AllocationsPanel adminApi={adminApi} />
        <ChargebacksPanel adminApi={adminApi} />
      </div>
    </div>
  );
}
