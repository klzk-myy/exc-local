/**
 * SessionList — Task 10.3.21 item 4 (GET/DELETE /api/v1/account/sessions).
 * Active sessions table (device, IP, geo, last-active, current marker)
 * with per-row revoke behind a confirmation modal, plus a revoke-all
 * control (Phase-12 Task 12.3.9: terminates all OTHER sessions).
 */
import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ConfirmModal } from '@/lib/input-helpers';
import { ErrorBox, btnGhost, cardCls, tableCls, tdCls, thCls } from '@/lib/ui';

import * as api from './api';
import { RequireAuth } from './guards';

function deviceLabel(s: api.SessionInfo): string {
  if (s.device !== undefined && s.device !== '') return s.device;
  if (s.user_agent !== undefined && s.user_agent !== '') return s.user_agent;
  return 'Unknown device';
}

export function SessionList() {
  const qc = useQueryClient();
  const sessions = useQuery({
    queryKey: ['account', 'sessions'],
    queryFn: () => api.listSessions(apiClient),
  });
  const [revoking, setRevoking] = useState<api.SessionInfo | null>(null);
  const [confirmAll, setConfirmAll] = useState(false);

  const revoke = useMutation({
    mutationFn: (id: string) => api.revokeSession(apiClient, id),
    onSuccess: () => {
      setRevoking(null);
      void qc.invalidateQueries({ queryKey: ['account', 'sessions'] });
    },
  });
  const revokeAll = useMutation({
    mutationFn: () => api.revokeAllSessions(apiClient),
    onSuccess: () => {
      setConfirmAll(false);
      void qc.invalidateQueries({ queryKey: ['account', 'sessions'] });
    },
  });

  return (
    <section className={cardCls} aria-labelledby="sessions-h">
      <div className="mb-3 flex items-center justify-between">
        <h2 id="sessions-h" className="text-sm font-medium text-neutral-300">
          Active sessions
        </h2>
        <button
          type="button"
          className={btnGhost}
          onClick={() => {
            setConfirmAll(true);
          }}
        >
          Sign out all other sessions
        </button>
      </div>
      <ErrorBox error={sessions.error ?? revoke.error ?? revokeAll.error} />
      {sessions.isLoading && <p className="text-sm text-neutral-500">Loading sessions…</p>}
      {sessions.data?.length === 0 && (
        <p className="text-sm text-neutral-500">No active sessions.</p>
      )}
      {sessions.data !== undefined && sessions.data.length > 0 && (
        <div className="relative overflow-x-auto" tabIndex={0}>
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>Device</th>
              <th className={thCls}>IP</th>
              <th className={thCls}>Location</th>
              <th className={thCls}>Last active</th>
              <th className={thCls}><span className="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            {sessions.data.map((s) => (
              <tr key={s.id}>
                <td className={tdCls}>
                  {deviceLabel(s)}
                  {s.current === true && (
                    <span className="ml-2 rounded bg-emerald-500/20 px-1.5 py-0.5 text-xs text-emerald-400">
                      current
                    </span>
                  )}
                </td>
                <td className={tdCls}>{s.ip ?? '—'}</td>
                <td className={tdCls}>
                  {[s.geo_city, s.geo_country]
                    .filter((v): v is string => v !== undefined)
                    .join(', ') || '—'}
                </td>
                <td className={tdCls}>{new Date(s.last_active_at).toLocaleString('en-US')}</td>
                <td className={tdCls}>
                  {s.current !== true && (
                    <button
                      type="button"
                      className={btnGhost}
                      onClick={() => {
                        setRevoking(s);
                      }}
                    >
                      Revoke
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        </div>
      )}

      <ConfirmModal
        open={revoking !== null}
        severity="LOW"
        title="Revoke session"
        confirmLabel="Revoke session"
        busy={revoke.isPending}
        onConfirm={() => {
          if (revoking !== null) revoke.mutate(revoking.id);
        }}
        onCancel={() => {
          setRevoking(null);
        }}
      >
        <p className="text-sm text-neutral-300">
          Sign out <strong>{revoking !== null ? deviceLabel(revoking) : ''}</strong>? The device
          loses access immediately.
        </p>
      </ConfirmModal>
      <ConfirmModal
        open={confirmAll}
        severity="MEDIUM"
        title="Sign out all other sessions"
        confirmLabel="Sign out all"
        busy={revokeAll.isPending}
        onConfirm={() => {
          revokeAll.mutate();
        }}
        onCancel={() => {
          setConfirmAll(false);
        }}
      >
        <p className="text-sm text-neutral-300">
          Every session except this one will be terminated. Devices will need to sign in again.
        </p>
      </ConfirmModal>
    </section>
  );
}

/** /account/sessions route component. */
export default function SessionsPage() {
  return (
    <RequireAuth>
      <div className="mx-auto max-w-3xl p-6">
        <h1 className="mb-4 text-xl font-semibold">Sessions &amp; devices</h1>
        <SessionList />
      </div>
    </RequireAuth>
  );
}
