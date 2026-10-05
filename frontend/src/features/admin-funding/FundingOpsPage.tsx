/**
 * Funding-ops console (Phase-10.5 Task 10.5.3.7) — admin work surfaces
 * for every money-movement review step: deposit ingest/confirm/review,
 * inbound-wire registration, rail returns, withdrawal approvals,
 * quarantine resolution, beneficiary verification and the ops-alerts
 * rail. Route: /admin/funding-ops.
 */
import { apiClient } from '@/app/runtime';
import { EnvPill, EnvSwitcher, EnvWatermark, useBoundAdminApi } from '@/lib/env';

import { RequireAdmin } from '../admin/RequireAdmin';
import { useAdminRole } from '../admin/adminRole';
import { BankAccountsPanel } from './BankAccountsPanel';
import { DepositOpsPanel } from './DepositOpsPanel';
import { OpsAlertsPanel } from './OpsAlertsPanel';
import { QuarantinePanel } from './QuarantinePanel';
import { WithdrawalPanel } from './WithdrawalPanel';

export default function FundingOpsPage() {
  return (
    <RequireAdmin>
      <FundingOpsDashboard />
    </RequireAdmin>
  );
}

function FundingOpsDashboard() {
  const adminApi = useBoundAdminApi(apiClient);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Funding operations</h1>
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
        <OpsAlertsPanel adminApi={adminApi} />
        <DepositOpsPanel adminApi={adminApi} />
        <WithdrawalPanel adminApi={adminApi} />
        <QuarantinePanel adminApi={adminApi} />
        <BankAccountsPanel adminApi={adminApi} />
      </div>
    </div>
  );
}
