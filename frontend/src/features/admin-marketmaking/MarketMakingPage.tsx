/**
 * MM program, DEA & algo governance console (Phase-10.5 Task
 * 10.5.3.12) — RTS 6 / DEA / market-maker program administration:
 * enrollment register with quota terms and suspend/resume/MMP-reset,
 * per-program compliance + rebate accruals + monthly GL sweep, algo
 * certifications, DEA session controls, RTS 6 self-assessments.
 * Route: /admin/market-making.
 */
import { apiClient } from '@/app/runtime';
import { EnvPill, EnvSwitcher, EnvWatermark, useBoundAdminApi } from '@/lib/env';

import { RequireAdmin } from '../admin/RequireAdmin';
import { useAdminRole } from '../admin/adminRole';
import { AlgoDeaPanel } from './AlgoDeaPanel';
import { MMProgramsPanel } from './MMProgramsPanel';

export default function MarketMakingPage() {
  return (
    <RequireAdmin>
      <MarketMakingDashboard />
    </RequireAdmin>
  );
}

function MarketMakingDashboard() {
  const adminApi = useBoundAdminApi(apiClient);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Market-making &amp; algo governance</h1>
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
        <MMProgramsPanel adminApi={adminApi} />
        <AlgoDeaPanel adminApi={adminApi} />
      </div>
    </div>
  );
}
