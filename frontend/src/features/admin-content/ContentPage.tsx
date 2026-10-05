/**
 * Content, promotions & emergency admin console (Phase-10.5 Task
 * 10.5.3.16) — announcement/maintenance CRUD, financial-promotion
 * lifecycle, strategy-template curation & copy-strategy suspend,
 * break-glass grants + review, API-key expiry extension, webhook
 * dead letters, and LP detail/alerts. Route: /admin/content.
 *
 * The dual-control queue itself is already served by DualControlPanel
 * on /admin — this surface submits into it (break-glass, key expiry)
 * rather than duplicating it.
 */
import { apiClient } from '@/app/runtime';
import { EnvPill, EnvSwitcher, EnvWatermark, useBoundAdminApi } from '@/lib/env';

import { RequireAdmin } from '../admin/RequireAdmin';
import { useAdminRole } from '../admin/adminRole';
import { ContentPanel } from './ContentPanel';
import { CurationPanel } from './CurationPanel';
import { EmergencyPanel } from './EmergencyPanel';
import { LpPanel } from './LpPanel';
import { PromotionsPanel } from './PromotionsPanel';

export default function ContentPage() {
  return (
    <RequireAdmin>
      <ContentDashboard />
    </RequireAdmin>
  );
}

function ContentDashboard() {
  const adminApi = useBoundAdminApi(apiClient);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Content &amp; emergency</h1>
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
        <ContentPanel adminApi={adminApi} />
        <PromotionsPanel adminApi={adminApi} />
        <CurationPanel adminApi={adminApi} />
        <EmergencyPanel adminApi={adminApi} />
        <LpPanel adminApi={adminApi} />
      </div>
    </div>
  );
}
