/**
 * Treasury, nostro & client-money console (Phase-10.5 Task 10.5.3.8) —
 * house-money and segregated-funds operations: nostro coverage /
 * registry / replenishment / SWIFT journal, nostro & PB
 * reconciliation, client-money assurance engagements + evidence packs
 * + segregation certifications, treasury own funds / contingent
 * capital / insurance fund / collateral schedule. Route: /admin/treasury.
 */
import { apiClient } from '@/app/runtime';
import { EnvPill, EnvSwitcher, EnvWatermark, useBoundAdminApi } from '@/lib/env';

import { RequireAdmin } from '../admin/RequireAdmin';
import { useAdminRole } from '../admin/adminRole';
import { ClientMoneyPanel } from './ClientMoneyPanel';
import { NostroPanel } from './NostroPanel';
import { ReconPanel } from './ReconPanel';
import { TreasuryPanel } from './TreasuryPanel';

export default function TreasuryPage() {
  return (
    <RequireAdmin>
      <TreasuryDashboard />
    </RequireAdmin>
  );
}

function TreasuryDashboard() {
  const adminApi = useBoundAdminApi(apiClient);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Treasury &amp; client money</h1>
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
        <NostroPanel adminApi={adminApi} />
        <ReconPanel adminApi={adminApi} />
        <ClientMoneyPanel adminApi={adminApi} />
        <TreasuryPanel adminApi={adminApi} />
      </div>
    </div>
  );
}
