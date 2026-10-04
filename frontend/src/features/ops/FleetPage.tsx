/**
 * Fleet page (Phase-10 Task 10.3.20 item 2) — env-scoped host inventory
 * + topology.
 *
 *   GET /api/v1/admin/fleet/hosts      ?state=&role=   (session env)
 *   GET /api/v1/admin/fleet/topology   ?env=           (session env)
 *   POST /api/v1/admin/fleet/hosts/{id}/{drain|cordon|decommission}
 *        — dual-control in production: the service demands a second
 *        approver (`approver_id` in body); DUAL_CONTROL_REQUIRED /
 *        FORBIDDEN surface via ErrorBox.
 *
 * Every call goes through the env-bound client — a fleet page rendered
 * for staging cannot reach production hosts.
 */
import { useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import type { ApiClient } from '@/lib/api';
import {
  fetchFleetHosts,
  fetchFleetTopology,
  fleetHostAction,
  type FleetHost,
} from '@/lib/admin/api';
import {
  EnvSwitcher,
  EnvWatermark,
  EnvPill,
  useBoundAdminApi,
  type BoundAdminApi,
} from '@/lib/env';
import {
  btnDanger,
  btnGhost,
  cardCls,
  inputCls,
  labelCls,
  tableCls,
  tdCls,
  thCls,
  ErrorBox,
  Modal,
  StatusBadge,
} from '@/lib/ui';
import { AccessDeniedCard, RequireAdmin } from '@/features/admin/RequireAdmin';
import { isAccessDenied, useAdminRole } from '@/features/admin/adminRole';

const HOST_ACTIONS = ['drain', 'cordon', 'decommission'] as const;
type HostAction = (typeof HOST_ACTIONS)[number];

/** §19.16.2 lifecycle preview shown in the confirm modal — drain:
 * ACTIVE→DRAINING, cordon: →MAINTENANCE, decommission: →DECOMMISSIONED
 * (terminal). */
const ACTION_PREVIEW: Record<HostAction, string> = {
  drain: 'ACTIVE → DRAINING — new work stops landing; the host drains then idles for maintenance.',
  cordon: '→ MAINTENANCE — the host is fenced off for service; reactivate returns it to ACTIVE.',
  decommission: '→ DECOMMISSIONED — terminal. The host leaves the fleet inventory permanently.',
};

function HostActionModal({
  host,
  action,
  env,
  onClose,
  onSubmit,
  busy,
}: {
  host: FleetHost;
  action: HostAction;
  env: string;
  onClose: () => void;
  onSubmit: (reason: string, approverId?: number) => void;
  busy: boolean;
}) {
  const [reason, setReason] = useState('');
  const [approver, setApprover] = useState('');
  const prod = env === 'production';
  const approverId = Number(approver);
  const valid = reason.trim() !== '' && (!prod || (Number.isInteger(approverId) && approverId > 0));
  return (
    <Modal open title={`${action} ${host.hostname}`} onClose={onClose}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (valid) onSubmit(reason.trim(), prod ? approverId : undefined);
        }}
      >
        <p className="mb-3 text-sm text-neutral-400">
          {action} is a dual-controlled fleet action
          {prod ? ' — production requires a distinct approver id' : ''}. The server enforces the
          interlock; this form only collects the evidence it needs.
        </p>
        <p className="mb-3 text-sm text-amber-300" data-testid="action-preview">
          Resulting state: {ACTION_PREVIEW[action]}
        </p>
        <label className={labelCls} htmlFor="ha-reason">
          Reason (required)
        </label>
        <input
          id="ha-reason"
          className={`${inputCls} mb-3`}
          value={reason}
          onChange={(e) => setReason(e.target.value)}
        />
        {prod && (
          <>
            <label className={labelCls} htmlFor="ha-approver">
              Approver admin id (second operator, required in production)
            </label>
            <input
              id="ha-approver"
              className={`${inputCls} mb-3`}
              value={approver}
              onChange={(e) => setApprover(e.target.value)}
            />
          </>
        )}
        <div className="flex justify-end gap-2">
          <button type="button" className={btnGhost} onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className={btnDanger} disabled={!valid || busy}>
            {busy ? 'Submitting…' : `Confirm ${action}`}
          </button>
        </div>
      </form>
    </Modal>
  );
}

function HostsTable({ adminApi, role }: { adminApi: BoundAdminApi; role: string | null }) {
  const qc = useQueryClient();
  const [stateFilter, setStateFilter] = useState('');
  const [roleFilter, setRoleFilter] = useState('');
  const [actionTarget, setActionTarget] = useState<{ host: FleetHost; action: HostAction } | null>(
    null,
  );
  const [actionBusy, setActionBusy] = useState(false);
  const [actionError, setActionError] = useState<unknown>(null);
  const [actionResult, setActionResult] = useState<string | null>(null);

  const query = useQuery({
    queryKey: ['ops', 'fleet-hosts', adminApi.env, stateFilter, roleFilter],
    queryFn: () =>
      fetchFleetHosts(adminApi, {
        state: stateFilter || undefined,
        role: roleFilter || undefined,
      }),
    retry: false,
    refetchInterval: 30_000,
  });

  const canAct = role === 'Super Admin';

  const submit = async (
    host: FleetHost,
    action: HostAction,
    reason: string,
    approverId?: number,
  ) => {
    setActionBusy(true);
    setActionError(null);
    setActionResult(null);
    try {
      const sa = await fleetHostAction(adminApi, host.id, action, { reason, approverId });
      setActionResult(
        sa !== null
          ? `${action} on ${host.hostname}: ${sa.status}`
          : `${action} on ${host.hostname}: submitted`,
      );
      setActionTarget(null);
      await qc.invalidateQueries({ queryKey: ['ops', 'fleet-hosts'] });
    } catch (e) {
      setActionError(e);
    } finally {
      setActionBusy(false);
    }
  };

  return (
    <section className={cardCls} aria-label="Host inventory">
      <div className="mb-2 flex flex-wrap items-center gap-2">
        <h2 className="text-sm font-medium text-neutral-400">Hosts — {adminApi.env}</h2>
        <input
          aria-label="State filter"
          placeholder="state (e.g. ACTIVE)"
          className={`${inputCls} w-40`}
          value={stateFilter}
          onChange={(e) => setStateFilter(e.target.value)}
        />
        <input
          aria-label="Role filter"
          placeholder="role (e.g. matcher)"
          className={`${inputCls} w-40`}
          value={roleFilter}
          onChange={(e) => setRoleFilter(e.target.value)}
        />
      </div>
      {query.error !== null && isAccessDenied(query.error) && (
        <AccessDeniedCard detail="Fleet inventory requires the Super Admin role for this env." />
      )}
      {query.error !== null && !isAccessDenied(query.error) && <ErrorBox error={query.error} />}
      <ErrorBox error={actionError} onDismiss={() => setActionError(null)} />
      {actionResult !== null && (
        <p className="mb-2 text-xs text-emerald-400" role="status" data-testid="host-action-result">
          {actionResult}
        </p>
      )}
      {query.error === null && (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>Host</th>
              <th className={thCls}>Role</th>
              <th className={thCls}>Shard</th>
              <th className={thCls}>AZ</th>
              <th className={thCls}>Health</th>
              <th className={thCls}>State</th>
              <th className={thCls}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {(query.data ?? []).map((h) => (
              <tr key={h.id}>
                <td className={tdCls}>{h.hostname}</td>
                <td className={tdCls}>{h.role}</td>
                <td className={tdCls}>{h.shardId ?? '—'}</td>
                <td className={tdCls}>{h.az ?? '—'}</td>
                <td className={tdCls}>
                  <StatusBadge value={h.health.toUpperCase()} />
                </td>
                <td className={tdCls}>
                  <StatusBadge value={h.state.toUpperCase()} />
                </td>
                <td className={tdCls}>
                  {canAct ? (
                    <span className="flex gap-1">
                      {HOST_ACTIONS.map((a) => (
                        <button
                          key={a}
                          type="button"
                          className={btnGhost}
                          title={`${a} — dual-control in production (server-enforced)`}
                          onClick={() => setActionTarget({ host: h, action: a })}
                        >
                          {a}
                        </button>
                      ))}
                    </span>
                  ) : (
                    <span className="text-xs text-neutral-600">Super Admin only</span>
                  )}
                </td>
              </tr>
            ))}
            {(query.data ?? []).length === 0 && !query.isLoading && (
              <tr>
                <td className={tdCls} colSpan={7}>
                  No hosts in this environment/filter.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      )}
      {actionTarget !== null && (
        <HostActionModal
          host={actionTarget.host}
          action={actionTarget.action}
          env={adminApi.env}
          busy={actionBusy}
          onClose={() => setActionTarget(null)}
          onSubmit={(reason, approverId) =>
            void submit(actionTarget.host, actionTarget.action, reason, approverId)
          }
        />
      )}
    </section>
  );
}

function TopologyPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const query = useQuery({
    queryKey: ['ops', 'fleet-topology', adminApi.env],
    queryFn: () => fetchFleetTopology(adminApi),
    retry: false,
  });
  return (
    <section className={cardCls} aria-label="Topology">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">Topology — {adminApi.env}</h2>
      {query.error !== null && isAccessDenied(query.error) && (
        <AccessDeniedCard detail="Fleet topology requires the Super Admin role." />
      )}
      {query.error !== null && !isAccessDenied(query.error) && <ErrorBox error={query.error} />}
      {query.data !== undefined && (
        <div className="grid gap-3 md:grid-cols-3">
          <div>
            <h3 className="mb-1 text-xs font-medium text-neutral-500">Shards</h3>
            <ul className="text-xs text-neutral-300">
              {Object.entries(query.data.shards).map(([shard, hosts]) => (
                <li key={shard}>
                  {shard}: {hosts.length} host(s) — {hosts.map((h) => h.hostname).join(', ')}
                </li>
              ))}
              {Object.keys(query.data.shards).length === 0 && <li>No shards reported.</li>}
            </ul>
          </div>
          <div>
            <h3 className="mb-1 text-xs font-medium text-neutral-500">Roles</h3>
            <ul className="text-xs text-neutral-300">
              {Object.entries(query.data.roles).map(([role, names]) => (
                <li key={role}>
                  {role}: {names.length}
                </li>
              ))}
              {Object.keys(query.data.roles).length === 0 && <li>No roles reported.</li>}
            </ul>
          </div>
          <div>
            <h3 className="mb-1 text-xs font-medium text-neutral-500">Health</h3>
            <ul className="text-xs text-neutral-300">
              {Object.entries(query.data.health).map(([state, count]) => (
                <li key={state}>
                  {state}: {count}
                </li>
              ))}
              {Object.keys(query.data.health).length === 0 && <li>No health data.</li>}
            </ul>
          </div>
        </div>
      )}
    </section>
  );
}

export default function FleetPage({ api = apiClient }: { api?: ApiClient }) {
  return (
    <RequireAdmin>
      <Fleet api={api} />
    </RequireAdmin>
  );
}

function Fleet({ api }: { api: ApiClient }) {
  const adminApi = useBoundAdminApi(api);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Fleet</h1>
        <div className="flex items-center gap-3">
          <EnvSwitcher />
          <EnvPill />
        </div>
      </div>
      <div className="space-y-4">
        <HostsTable adminApi={adminApi} role={role} />
        <TopologyPanel adminApi={adminApi} />
      </div>
    </div>
  );
}
