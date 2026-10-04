/**
 * IP-security panel (Phase-10.5 Task 10.5.3.1 §4) — progressive ban
 * review/override over the Task 5.3.34 backend. Manual bans, unban and
 * pardon (strike reset), allowlist exemptions, and the ban audit tail.
 * The escalation ladder (2min → 30min → 24h) is displayed per row.
 */
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import { apiClient } from '@/app/runtime';
import type { BoundAdminApi } from '@/lib/env';
import {
  btnDanger,
  btnGhost,
  cardCls,
  ConfirmAction,
  ErrorBox,
  hintTextCls,
  inputCls,
  labelCls,
  Modal,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { AccessDeniedCard } from '../admin/RequireAdmin';
import { isAccessDenied } from '../admin/adminRole';
import { allowlistIp, banIp, fetchIpBanAudit, fetchIpBans, unallowlistIp, unbanIp } from './api';

const fmtMs = (ms: number): string => (ms > 0 ? new Date(ms).toLocaleString() : '—');

export function IpSecurityPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [ip, setIp] = useState('');
  const [durationS, setDurationS] = useState('7200');
  const [reason, setReason] = useState('');
  const [actionError, setActionError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const [confirm, setConfirm] = useState<{
    ip: string;
    action: 'unban' | 'pardon' | 'allowlist';
  } | null>(null);
  const [allowTarget, setAllowTarget] = useState('');
  const [showAudit, setShowAudit] = useState(false);

  const bansQuery = useQuery({
    queryKey: ['admin-ops', 'ip-bans', adminApi.env],
    queryFn: () => fetchIpBans(adminApi),
    retry: false,
    refetchInterval: 30_000,
  });
  const auditQuery = useQuery({
    queryKey: ['admin-ops', 'ip-bans-audit', adminApi.env],
    queryFn: () => fetchIpBanAudit(adminApi, 50),
    retry: false,
    enabled: showAudit,
  });

  if (isAccessDenied(bansQuery.error)) return <AccessDeniedCard />;

  const invalidate = () => qc.invalidateQueries({ queryKey: ['admin-ops', 'ip-bans'] });

  const doBan = async () => {
    setBusy(true);
    setActionError(null);
    try {
      await banIp(apiClient, adminApi.env, {
        ip: ip.trim(),
        durationS: Number(durationS) || 0,
        reason: reason.trim(),
      });
      setIp('');
      setReason('');
      await invalidate();
    } catch (e) {
      setActionError(e);
    } finally {
      setBusy(false);
    }
  };

  const doConfirm = async () => {
    if (confirm === null) return;
    setBusy(true);
    setActionError(null);
    try {
      if (confirm.action === 'unban') await unbanIp(apiClient, adminApi.env, confirm.ip, false);
      if (confirm.action === 'pardon') await unbanIp(apiClient, adminApi.env, confirm.ip, true);
      if (confirm.action === 'allowlist') await allowlistIp(apiClient, adminApi.env, confirm.ip);
      setConfirm(null);
      await invalidate();
    } catch (e) {
      setActionError(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className={cardCls} aria-label="IP security">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">IP bans &amp; allowlist</h2>
      <p className={hintTextCls}>
        Progressive ladder — strike 1: 2min, strike 2: 30min, strike ≥3: 24h (HTTP 418). Pardon
        clears the ban AND the strike history; allowlist exempts an IP from the machinery.
      </p>

      {bansQuery.isError && <ErrorBox error={bansQuery.error} />}
      {actionError !== null && <ErrorBox error={actionError} />}

      <div className="mb-3 grid gap-2 sm:grid-cols-2 lg:grid-cols-4">
        <div>
          <label className={labelCls} htmlFor="ip-ban-target">
            IP to ban
          </label>
          <input
            id="ip-ban-target"
            className={inputCls}
            value={ip}
            placeholder="203.0.113.7"
            onChange={(e) => setIp(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="ip-ban-duration">
            Duration (seconds)
          </label>
          <input
            id="ip-ban-duration"
            className={inputCls}
            inputMode="numeric"
            value={durationS}
            onChange={(e) => setDurationS(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="ip-ban-reason">
            Reason
          </label>
          <input
            id="ip-ban-reason"
            className={inputCls}
            value={reason}
            onChange={(e) => setReason(e.target.value)}
          />
        </div>
        <div className="flex items-end">
          <button
            type="button"
            className={btnDanger}
            disabled={busy || ip.trim() === ''}
            onClick={() => void doBan()}
          >
            Ban IP
          </button>
        </div>
      </div>

      <div className="mb-3 flex flex-wrap items-end gap-2">
        <div>
          <label className={labelCls} htmlFor="ip-allow-target">
            Allowlist IP
          </label>
          <input
            id="ip-allow-target"
            className={inputCls}
            value={allowTarget}
            onChange={(e) => setAllowTarget(e.target.value)}
          />
        </div>
        <button
          type="button"
          className={btnGhost}
          disabled={busy || allowTarget.trim() === ''}
          onClick={() => setConfirm({ ip: allowTarget.trim(), action: 'allowlist' })}
        >
          Allowlist
        </button>
        <button
          type="button"
          className={btnGhost}
          disabled={busy || allowTarget.trim() === ''}
          onClick={() =>
            void (async () => {
              setBusy(true);
              setActionError(null);
              try {
                await unallowlistIp(apiClient, adminApi.env, allowTarget.trim());
              } catch (e) {
                setActionError(e);
              } finally {
                setBusy(false);
              }
            })()
          }
        >
          Remove allowlist
        </button>
        <button type="button" className={btnGhost} onClick={() => setShowAudit((s) => !s)}>
          {showAudit ? 'Hide audit' : 'Show audit'}
        </button>
      </div>

      {bansQuery.isSuccess &&
        (bansQuery.data.length === 0 ? (
          <p className="text-sm text-neutral-500">No active bans.</p>
        ) : (
          <div className="overflow-x-auto">
            <table className={tableCls}>
              <thead>
                <tr>
                  <th className={thCls}>IP</th>
                  <th className={thCls}>Level</th>
                  <th className={thCls}>Strikes</th>
                  <th className={thCls}>Reason</th>
                  <th className={thCls}>Expires</th>
                  <th className={thCls}>Actor</th>
                  <th className={thCls}>
                    <span className="sr-only">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {bansQuery.data.map((b) => (
                  <tr key={b.ip}>
                    <td className={tdCls}>{b.ip}</td>
                    <td className={tdCls}>{b.level}</td>
                    <td className={tdCls}>{b.strikes}</td>
                    <td className={tdCls}>{b.reason || '—'}</td>
                    <td className={tdCls}>{fmtMs(b.expiresAtMs)}</td>
                    <td className={tdCls}>{b.actor || '—'}</td>
                    <td className={tdCls}>
                      <div className="flex gap-1">
                        <button
                          type="button"
                          className={btnGhost}
                          disabled={busy}
                          onClick={() => setConfirm({ ip: b.ip, action: 'unban' })}
                        >
                          Unban
                        </button>
                        <button
                          type="button"
                          className={btnGhost}
                          disabled={busy}
                          onClick={() => setConfirm({ ip: b.ip, action: 'pardon' })}
                        >
                          Pardon
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ))}

      {showAudit && (
        <div className="mt-3">
          <h3 className="mb-1 text-xs font-medium text-neutral-400">Ban audit (latest 50)</h3>
          {auditQuery.isError && <ErrorBox error={auditQuery.error} />}
          {auditQuery.isSuccess &&
            (auditQuery.data.length === 0 ? (
              <p className="text-sm text-neutral-500">No audit events.</p>
            ) : (
              <ul className="max-h-48 space-y-1 overflow-y-auto font-mono text-xs text-neutral-400">
                {auditQuery.data.map((row, i) => (
                  <li key={i} className="break-all">
                    {typeof row === 'string' ? row : JSON.stringify(row)}
                  </li>
                ))}
              </ul>
            ))}
        </div>
      )}

      <Modal open={confirm !== null} title="Confirm IP action" onClose={() => setConfirm(null)}>
        {confirm !== null && (
          <ConfirmAction
            message={
              confirm.action === 'allowlist' ? (
                <>
                  Allowlist <strong>{confirm.ip}</strong>? The ban machinery will ignore this IP.
                </>
              ) : confirm.action === 'pardon' ? (
                <>
                  Pardon <strong>{confirm.ip}</strong>? Removes the ban AND resets the strike
                  history — the next offense restarts at level 1.
                </>
              ) : (
                <>
                  Unban <strong>{confirm.ip}</strong>? Strike history is kept — the next offense
                  continues the ladder.
                </>
              )
            }
            confirmLabel={
              confirm.action === 'allowlist'
                ? 'Allowlist'
                : confirm.action === 'pardon'
                  ? 'Pardon'
                  : 'Unban'
            }
            danger={confirm.action !== 'allowlist'}
            busy={busy}
            onCancel={() => setConfirm(null)}
            onConfirm={() => void doConfirm()}
          />
        )}
      </Modal>
    </section>
  );
}
