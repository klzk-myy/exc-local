/**
 * Case desk console (Phase-10.5 Task 10.5.3.6) — the surveillance →
 * case → disposition → SAR pipeline UI plus AML program register,
 * sanctions provider ops and MiFID II comms recordings (dual-controlled
 * WORM retrieval). Route: /admin/surveillance.
 */
import { apiClient } from '@/app/runtime';
import { EnvPill, EnvSwitcher, EnvWatermark, useBoundAdminApi } from '@/lib/env';

import { RequireAdmin } from '../admin/RequireAdmin';
import { useAdminRole } from '../admin/adminRole';
import { AmlPanel } from './AmlPanel';
import { CommsPanel } from './CommsPanel';
import { SanctionsOpsPanel } from './SanctionsOpsPanel';
import { SarCtrPanel } from './SarCtrPanel';
import { SurveillancePanel } from './SurveillancePanel';
import { TuningPanel } from './TuningPanel';

export default function CaseDeskPage() {
  return (
    <RequireAdmin>
      <CaseDeskDashboard />
    </RequireAdmin>
  );
}

function CaseDeskDashboard() {
  const adminApi = useBoundAdminApi(apiClient);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Surveillance &amp; AML</h1>
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
        <SurveillancePanel adminApi={adminApi} />
        <SarCtrPanel adminApi={adminApi} />
        <AmlPanel adminApi={adminApi} />
        <SanctionsOpsPanel adminApi={adminApi} />
        <TuningPanel adminApi={adminApi} />
        <CommsPanel adminApi={adminApi} />
      </div>
    </div>
  );
}
