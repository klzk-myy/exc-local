/**
 * Kill-switch panel (Phase-10.5 Task 10.5.3.1 §1) — the venue's master
 * halt lever over the Phase-11 backend.
 *
 * GET /admin/kill-switch lists ACTIVE suspensions; POST trips a scope,
 * POST /reset clears it. GLOBAL and COUNTERPARTY scopes are four-eyes
 * on the server (a distinct approver_id is mandatory); every trip and
 * reset lands in trading_suspensions + admin_audit_log.
 */
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnDanger,
  btnGhost,
  cardCls,
  ErrorBox,
  hintTextCls,
  inputCls,
  labelCls,
  Modal,
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import {
  fetchSuspensions,
  KILL_SWITCH_SCOPES,
  resetKillSwitch,
  tripKillSwitch,
  type KillSwitchScope,
  type Suspension,
} from './api';

const FOUR_EYES_SCOPES: ReadonlySet<string> = new Set(['GLOBAL', 'COUNTERPARTY']);

export function KillSwitchPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [scope, setScope] = useState<KillSwitchScope>('INSTRUMENT');
  const [targetId, setTargetId] = useState('');
  const [reason, setReason] = useState('');
  const [approverId, setApproverId] = useState('');
  const [actionError, setActionError] = useState<unknown>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [resetTarget, setResetTarget] = useState<Suspension | null>(null);
  const [resetReason, setResetReason] = useState('');
  const [resetApprover, setResetApprover] = useState('');

  const query = useQuery({
    queryKey: ['admin-ops', 'kill-switch', adminApi.env],
    queryFn: () => fetchSuspensions(adminApi),
    retry: false,
    refetchInterval: 15_000,
  });

  const needsTarget = scope !== 'GLOBAL';
  const needsApprover = FOUR_EYES_SCOPES.has(scope);

  const trip = async () => {
    setBusy(true);
    setActionError(null);
    setNotice(null);
    try {
      await tripKillSwitch(adminApi, {
        scope,
        targetId: needsTarget ? targetId.trim() : '',
        reason: reason.trim(),
        approverId: Number(approverId) || 0,
      });
      setNotice(`Suspension recorded for ${scope}${needsTarget ? `:${targetId.trim()}` : ''}`);
      setReason('');
      setTargetId('');
      await qc.invalidateQueries({ queryKey: ['admin-ops', 'kill-switch'] });
    } catch (e) {
      setActionError(e);
    } finally {
      setBusy(false);
    }
  };

  const doReset = async () => {
    if (resetTarget === null) return;
    setBusy(true);
    setActionError(null);
    try {
      await resetKillSwitch(adminApi, {
        scope: resetTarget.scope as KillSwitchScope,
        targetId: resetTarget.targetId,
        reason: resetReason.trim(),
        approverId: Number(resetApprover) || 0,
      });
      setNotice(`Suspension ${resetTarget.suspensionId} cleared`);
      setResetTarget(null);
      setResetReason('');
      setResetApprover('');
      await qc.invalidateQueries({ queryKey: ['admin-ops', 'kill-switch'] });
    } catch (e) {
      setActionError(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className={cardCls} aria-label="Kill switch">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">Kill switch</h2>
      <p className={hintTextCls}>
        Trips a trading suspension; new orders reject TRADING_HALTED while active. GLOBAL and
        COUNTERPARTY require a distinct approver (four-eyes, enforced server-side).
      </p>

      {query.isError && <ErrorBox error={query.error} />}
      {actionError !== null && <ErrorBox error={actionError} />}
      {notice !== null && (
        <p className="mb-2 text-sm text-emerald-300" role="status">
          {notice}
        </p>
      )}

      <div className="mb-3 grid gap-2 sm:grid-cols-2 lg:grid-cols-5">
        <div>
          <label className={labelCls} htmlFor="ks-scope">
            Scope
          </label>
          <select
            id="ks-scope"
            className={selectCls}
            value={scope}
            onChange={(e) => setScope(e.target.value as KillSwitchScope)}
          >
            {KILL_SWITCH_SCOPES.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </div>
        <div>
          <label className={labelCls} htmlFor="ks-target">
            Target {needsTarget ? '(required)' : '(unused for GLOBAL)'}
          </label>
          <input
            id="ks-target"
            className={inputCls}
            value={targetId}
            disabled={!needsTarget}
            placeholder={needsTarget ? 'account id / symbol / rail…' : '—'}
            onChange={(e) => setTargetId(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="ks-reason">
            Reason
          </label>
          <input
            id="ks-reason"
            className={inputCls}
            value={reason}
            onChange={(e) => setReason(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="ks-approver">
            Approver id {needsApprover ? '(required)' : '(optional)'}
          </label>
          <input
            id="ks-approver"
            className={inputCls}
            inputMode="numeric"
            value={approverId}
            onChange={(e) => setApproverId(e.target.value)}
          />
        </div>
        <div className="flex items-end">
          <button
            type="button"
            className={btnDanger}
            disabled={
              busy ||
              reason.trim() === '' ||
              (needsTarget && targetId.trim() === '') ||
              (needsApprover && approverId.trim() === '')
            }
            onClick={() => void trip()}
          >
            Trip kill switch
          </button>
        </div>
      </div>

      {query.isSuccess &&
        (query.data.length === 0 ? (
          <p className="text-sm text-neutral-500">No active suspensions.</p>
        ) : (
          <div className="relative overflow-x-auto" tabIndex={0}>
            <table className={tableCls}>
              <thead>
                <tr>
                  <th className={thCls}>ID</th>
                  <th className={thCls}>Scope</th>
                  <th className={thCls}>Target</th>
                  <th className={thCls}>Reason</th>
                  <th className={thCls}>Initiated by</th>
                  <th className={thCls}>State</th>
                  <th className={thCls}>
                    <span className="sr-only">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {query.data.map((s) => (
                  <tr key={s.suspensionId}>
                    <td className={tdCls}>{s.suspensionId}</td>
                    <td className={tdCls}>{s.scope}</td>
                    <td className={tdCls}>{s.targetId || '—'}</td>
                    <td className={tdCls}>{s.reason || '—'}</td>
                    <td className={tdCls}>{s.initiatedBy}</td>
                    <td className={tdCls}>
                      <StatusBadge value={s.state} />
                    </td>
                    <td className={tdCls}>
                      {s.state === 'ACTIVE' && (
                        <button
                          type="button"
                          className={btnGhost}
                          onClick={() => setResetTarget(s)}
                        >
                          Reset
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ))}

      <Modal
        open={resetTarget !== null}
        title="Reset suspension"
        onClose={() => setResetTarget(null)}
      >
        {resetTarget !== null && (
          <div className="space-y-3">
            <p className="text-sm text-neutral-300">
              Clear suspension{' '}
              <strong>
                {resetTarget.scope}:{resetTarget.targetId}
              </strong>
              ? Trading resumes for this scope immediately.
            </p>
            <div>
              <label className={labelCls} htmlFor="ks-reset-reason">
                Reason
              </label>
              <input
                id="ks-reset-reason"
                className={inputCls}
                value={resetReason}
                onChange={(e) => setResetReason(e.target.value)}
              />
            </div>
            <div>
              <label className={labelCls} htmlFor="ks-reset-approver">
                Approver id {FOUR_EYES_SCOPES.has(resetTarget.scope) ? '(required)' : '(optional)'}
              </label>
              <input
                id="ks-reset-approver"
                className={inputCls}
                inputMode="numeric"
                value={resetApprover}
                onChange={(e) => setResetApprover(e.target.value)}
              />
            </div>
            <div className="flex justify-end gap-2">
              <button type="button" className={btnGhost} onClick={() => setResetTarget(null)}>
                Cancel
              </button>
              <button
                type="button"
                className={btnDanger}
                disabled={
                  busy || (FOUR_EYES_SCOPES.has(resetTarget.scope) && resetApprover.trim() === '')
                }
                onClick={() => void doReset()}
              >
                {busy ? 'Working…' : 'Reset suspension'}
              </button>
            </div>
          </div>
        )}
      </Modal>
    </section>
  );
}
