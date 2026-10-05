/**
 * Finance, fees & tax-reporting admin console (Phase-10.5 Task
 * 10.5.3.10 — Batch C closeout) — pricing and house-finance
 * administration: versioned funding fee schedules, dual-controlled
 * promo windows, trial balance / P&L / balance-sheet / invoices /
 * reporting-values, and the CRS/FATCA report-run lifecycle.
 * Route: /admin/finance.
 */
import { apiClient } from '@/app/runtime';
import { EnvPill, EnvSwitcher, EnvWatermark, useBoundAdminApi } from '@/lib/env';

import { RequireAdmin } from '../admin/RequireAdmin';
import { useAdminRole } from '../admin/adminRole';
import { FeeSchedulesPanel } from './FeeSchedulesPanel';
import { FinancePanel } from './FinancePanel';
import { PromosPanel } from './PromosPanel';
import { TaxReportingPanel } from './TaxReportingPanel';

export default function FinancePage() {
  return (
    <RequireAdmin>
      <FinanceDashboard />
    </RequireAdmin>
  );
}

function FinanceDashboard() {
  const adminApi = useBoundAdminApi(apiClient);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Finance, fees &amp; tax reporting</h1>
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
        <FeeSchedulesPanel adminApi={adminApi} />
        <PromosPanel adminApi={adminApi} />
        <FinancePanel adminApi={adminApi} />
        <TaxReportingPanel adminApi={adminApi} />
      </div>
    </div>
  );
}
