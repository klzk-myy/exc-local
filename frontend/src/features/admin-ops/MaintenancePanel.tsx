/**
 * Maintenance-window panel (Phase-10.5 Task 10.5.3.1 §5) — the admin
 * CRUD half of the Task 5.3.14 backend; the public status surface
 * renders SCHEDULED/IN_PROGRESS windows from the same store
 * (Task 10.5.3.23's status page reads them).
 */
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import { apiClient } from '@/app/runtime';
import type { BoundAdminApi } from '@/lib/env';
import {
  btnDanger,
  btnPrimary,
  cardCls,
  ConfirmAction,
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

import { AccessDeniedCard } from '../admin/RequireAdmin';
import { isAccessDenied } from '../admin/adminRole';
import {
  createMaintenanceWindow,
  deleteMaintenanceWindow,
  fetchMaintenanceWindows,
  MAINTENANCE_SCOPES,
  MAINTENANCE_STATUSES,
  updateMaintenanceWindow,
  type MaintenanceWindow,
} from './api';

const toRfc3339 = (local: string): string => {
  // datetime-local → RFC3339 UTC; empty stays empty (validation upstream).
  if (local === '') return '';
  const d = new Date(local);
  return Number.isNaN(d.getTime()) ? local : d.toISOString();
};

export function MaintenancePanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [title, setTitle] = useState('');
  const [scope, setScope] = useState<string>(MAINTENANCE_SCOPES[0]);
  const [startsAt, setStartsAt] = useState('');
  const [endsAt, setEndsAt] = useState('');
  const [description, setDescription] = useState('');
  const [actionError, setActionError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<MaintenanceWindow | null>(null);

  const query = useQuery({
    queryKey: ['admin-ops', 'maintenance', adminApi.env],
    queryFn: () => fetchMaintenanceWindows(adminApi),
    retry: false,
    refetchInterval: 60_000,
  });

  if (isAccessDenied(query.error)) return <AccessDeniedCard />;

  const invalidate = () => qc.invalidateQueries({ queryKey: ['admin-ops', 'maintenance'] });

  const create = async () => {
    setBusy(true);
    setActionError(null);
    try {
      await createMaintenanceWindow(adminApi, {
        title: title.trim(),
        description: description.trim() || undefined,
        scope,
        startsAt: toRfc3339(startsAt),
        endsAt: toRfc3339(endsAt),
        status: 'SCHEDULED',
      });
      setTitle('');
      setDescription('');
      setStartsAt('');
      setEndsAt('');
      await invalidate();
    } catch (e) {
      setActionError(e);
    } finally {
      setBusy(false);
    }
  };

  const setStatus = async (w: MaintenanceWindow, status: string) => {
    setBusy(true);
    setActionError(null);
    try {
      await updateMaintenanceWindow(apiClient, adminApi.env, w.id, { status });
      await invalidate();
    } catch (e) {
      setActionError(e);
    } finally {
      setBusy(false);
    }
  };

  const doDelete = async () => {
    if (deleteTarget === null) return;
    setBusy(true);
    setActionError(null);
    try {
      await deleteMaintenanceWindow(apiClient, adminApi.env, deleteTarget.id);
      setDeleteTarget(null);
      await invalidate();
    } catch (e) {
      setActionError(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className={cardCls} aria-label="Maintenance windows">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">Maintenance windows</h2>
      <p className={hintTextCls}>
        Scheduled windows surface on the public status page and in venue banners while IN_PROGRESS.
      </p>

      {query.isError && <ErrorBox error={query.error} />}
      {actionError !== null && <ErrorBox error={actionError} />}

      <div className="mb-3 grid gap-2 sm:grid-cols-2 lg:grid-cols-6">
        <div className="lg:col-span-2">
          <label className={labelCls} htmlFor="mw-title">
            Title
          </label>
          <input
            id="mw-title"
            className={inputCls}
            value={title}
            onChange={(e) => setTitle(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="mw-scope">
            Scope
          </label>
          <select
            id="mw-scope"
            className={selectCls}
            value={scope}
            onChange={(e) => setScope(e.target.value)}
          >
            {MAINTENANCE_SCOPES.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </div>
        <div>
          <label className={labelCls} htmlFor="mw-start">
            Starts at
          </label>
          <input
            id="mw-start"
            type="datetime-local"
            className={inputCls}
            value={startsAt}
            onChange={(e) => setStartsAt(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="mw-end">
            Ends at
          </label>
          <input
            id="mw-end"
            type="datetime-local"
            className={inputCls}
            value={endsAt}
            onChange={(e) => setEndsAt(e.target.value)}
          />
        </div>
        <div className="flex items-end">
          <button
            type="button"
            className={btnPrimary}
            disabled={busy || title.trim() === '' || startsAt === '' || endsAt === ''}
            onClick={() => void create()}
          >
            Schedule
          </button>
        </div>
      </div>
      <div className="mb-3">
        <label className={labelCls} htmlFor="mw-desc">
          Description (optional)
        </label>
        <input
          id="mw-desc"
          className={inputCls}
          value={description}
          onChange={(e) => setDescription(e.target.value)}
        />
      </div>

      {query.isSuccess &&
        (query.data.length === 0 ? (
          <p className="text-sm text-neutral-500">No maintenance windows.</p>
        ) : (
          <div className="relative overflow-x-auto" tabIndex={0}>
            <table className={tableCls}>
              <thead>
                <tr>
                  <th className={thCls}>Title</th>
                  <th className={thCls}>Scope</th>
                  <th className={thCls}>Window</th>
                  <th className={thCls}>Status</th>
                  <th className={thCls}>
                    <span className="sr-only">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {query.data.map((w) => (
                  <tr key={w.id}>
                    <td className={tdCls}>
                      <div className="font-medium">{w.title}</div>
                      {w.description !== undefined && (
                        <div className="text-xs text-neutral-500">{w.description}</div>
                      )}
                    </td>
                    <td className={tdCls}>{w.scope ?? '—'}</td>
                    <td className={tdCls}>
                      {w.startsAt ?? '—'} → {w.endsAt ?? '—'}
                    </td>
                    <td className={tdCls}>
                      <StatusBadge value={w.status ?? 'SCHEDULED'} />
                    </td>
                    <td className={tdCls}>
                      <div className="flex flex-wrap items-center gap-1">
                        <select
                          aria-label={`Set status for ${w.title}`}
                          className={selectCls}
                          value={w.status ?? 'SCHEDULED'}
                          disabled={busy}
                          onChange={(e) => void setStatus(w, e.target.value)}
                        >
                          {MAINTENANCE_STATUSES.map((s) => (
                            <option key={s} value={s}>
                              {s}
                            </option>
                          ))}
                        </select>
                        <button
                          type="button"
                          className={btnDanger}
                          disabled={busy}
                          onClick={() => setDeleteTarget(w)}
                        >
                          Delete
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ))}

      <Modal
        open={deleteTarget !== null}
        title="Delete maintenance window"
        onClose={() => setDeleteTarget(null)}
      >
        {deleteTarget !== null && (
          <ConfirmAction
            message={
              <>
                Delete window <strong>{deleteTarget.title}</strong>? Any live status banners for it
                disappear immediately.
              </>
            }
            confirmLabel="Delete window"
            busy={busy}
            onCancel={() => setDeleteTarget(null)}
            onConfirm={() => void doDelete()}
          />
        )}
      </Modal>
    </section>
  );
}
