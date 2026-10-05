/**
 * Feature-flags panel (Phase-10.5 Task 10.5.3.1 §3) — CRUD + canary
 * ladder over the Phase-09 Task 9.3.7 backend. Toggle and delete are
 * confirm-modaled; every mutation lands in admin_audit_log server-side.
 */
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import { apiClient } from '@/app/runtime';
import type { BoundAdminApi } from '@/lib/env';
import {
  btnDanger,
  btnGhost,
  btnPrimary,
  cardCls,
  ConfirmAction,
  ErrorBox,
  hintTextCls,
  inputCls,
  labelCls,
  Modal,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { AccessDeniedCard } from '../admin/RequireAdmin';
import { isAccessDenied } from '../admin/adminRole';
import {
  advanceFlag,
  createFlag,
  deleteFlag,
  fetchFlag,
  fetchFlags,
  toggleFlag,
  updateFlag,
} from './api';

export function FlagsPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [newName, setNewName] = useState('');
  const [newDesc, setNewDesc] = useState('');
  const [actionError, setActionError] = useState<unknown>(null);
  const [busyName, setBusyName] = useState<string | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const [editName, setEditName] = useState<string | null>(null);
  const [editPct, setEditPct] = useState('');
  const [editDesc, setEditDesc] = useState('');

  const query = useQuery({
    queryKey: ['admin-ops', 'flags', adminApi.env],
    queryFn: () => fetchFlags(adminApi),
    retry: false,
  });

  if (isAccessDenied(query.error)) return <AccessDeniedCard />;

  const run = async (name: string, fn: () => Promise<unknown>) => {
    setBusyName(name);
    setActionError(null);
    try {
      await fn();
      await qc.invalidateQueries({ queryKey: ['admin-ops', 'flags'] });
    } catch (e) {
      setActionError(e);
    } finally {
      setBusyName(null);
    }
  };

  return (
    <section className={cardCls} aria-label="Feature flags">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">Feature flags</h2>
      <p className={hintTextCls}>
        Rollout state and canary ladders. Advance steps stages sequentially; delete removes the flag
        entirely (audited).
      </p>

      {query.isError && <ErrorBox error={query.error} />}
      {actionError !== null && <ErrorBox error={actionError} />}

      <div className="mb-3 flex flex-wrap items-end gap-2">
        <div>
          <label className={labelCls} htmlFor="flag-name">
            New flag name
          </label>
          <input
            id="flag-name"
            className={inputCls}
            value={newName}
            onChange={(e) => setNewName(e.target.value)}
          />
        </div>
        <div className="min-w-48 flex-1">
          <label className={labelCls} htmlFor="flag-desc">
            Description
          </label>
          <input
            id="flag-desc"
            className={inputCls}
            value={newDesc}
            onChange={(e) => setNewDesc(e.target.value)}
          />
        </div>
        <button
          type="button"
          className={btnPrimary}
          disabled={busyName !== null || newName.trim() === ''}
          onClick={() =>
            void run(newName.trim(), async () => {
              await createFlag(adminApi, {
                name: newName.trim(),
                enabled: false,
                description: newDesc.trim() || undefined,
              });
              setNewName('');
              setNewDesc('');
            })
          }
        >
          Create (disabled)
        </button>
      </div>

      {query.isSuccess &&
        (query.data.length === 0 ? (
          <p className="text-sm text-neutral-500">No flags defined.</p>
        ) : (
          <div className="overflow-x-auto">
            <table className={tableCls}>
              <thead>
                <tr>
                  <th className={thCls}>Name</th>
                  <th className={thCls}>State</th>
                  <th className={thCls}>Rollout</th>
                  <th className={thCls}>Stage</th>
                  <th className={thCls}>
                    <span className="sr-only">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {query.data.map((f) => (
                  <tr key={f.name}>
                    <td className={tdCls}>
                      <div className="font-medium">{f.name}</div>
                      {f.description !== undefined && (
                        <div className="text-xs text-neutral-500">{f.description}</div>
                      )}
                    </td>
                    <td className={tdCls}>
                      <StatusBadge value={f.enabled ? 'ENABLED' : 'DISABLED'} />
                    </td>
                    <td className={tdCls}>
                      {f.rolloutPct !== undefined ? `${f.rolloutPct}%` : '—'}
                    </td>
                    <td className={tdCls}>
                      {f.stages !== undefined && f.stages.length > 0
                        ? `${(f.stageIdx ?? -1) + 1}/${f.stages.length}`
                        : '—'}
                    </td>
                    <td className={tdCls}>
                      <div className="flex flex-wrap gap-1">
                        <button
                          type="button"
                          className={btnGhost}
                          disabled={busyName === f.name}
                          onClick={() =>
                            void run(f.name, () => toggleFlag(adminApi, f.name, !f.enabled))
                          }
                        >
                          {f.enabled ? 'Disable' : 'Enable'}
                        </button>
                        {f.stages !== undefined && f.stages.length > 0 && (
                          <button
                            type="button"
                            className={btnGhost}
                            disabled={busyName === f.name}
                            onClick={() => void run(f.name, () => advanceFlag(adminApi, f.name))}
                          >
                            Advance
                          </button>
                        )}
                        <button
                          type="button"
                          className={btnGhost}
                          disabled={busyName === f.name}
                          onClick={() =>
                            void run(f.name, async () => {
                              const fresh = await fetchFlag(adminApi, f.name);
                              setEditName(f.name);
                              setEditPct(
                                fresh.rolloutPct !== undefined ? String(fresh.rolloutPct) : '',
                              );
                              setEditDesc(fresh.description ?? '');
                            })
                          }
                        >
                          Edit
                        </button>
                        <button
                          type="button"
                          className={btnDanger}
                          disabled={busyName === f.name}
                          onClick={() => setDeleteTarget(f.name)}
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

      <Modal open={editName !== null} title="Edit flag" onClose={() => setEditName(null)}>
        {editName !== null && (
          <form
            className="space-y-3"
            onSubmit={(e) => {
              e.preventDefault();
              void run(editName, async () => {
                await updateFlag(adminApi, editName, {
                  ...(editPct.trim() !== '' ? { rolloutPct: Number(editPct) } : {}),
                  description: editDesc.trim() === '' ? undefined : editDesc.trim(),
                });
                setEditName(null);
              });
            }}
          >
            <div>
              <label className={labelCls} htmlFor="flag-edit-pct">
                Rollout % (blank = unchanged)
              </label>
              <input
                id="flag-edit-pct"
                className={inputCls}
                type="number"
                min={0}
                max={100}
                value={editPct}
                onChange={(e) => setEditPct(e.target.value)}
              />
            </div>
            <div>
              <label className={labelCls} htmlFor="flag-edit-desc">
                Description
              </label>
              <input
                id="flag-edit-desc"
                className={inputCls}
                value={editDesc}
                onChange={(e) => setEditDesc(e.target.value)}
              />
            </div>
            <div className="flex gap-2">
              <button
                type="button"
                className={btnGhost}
                onClick={() => {
                  setEditName(null);
                }}
              >
                Cancel
              </button>
              <button type="submit" className={btnPrimary} disabled={busyName === editName}>
                Save
              </button>
            </div>
          </form>
        )}
      </Modal>

      <Modal open={deleteTarget !== null} title="Delete flag" onClose={() => setDeleteTarget(null)}>
        {deleteTarget !== null && (
          <ConfirmAction
            message={
              <>
                Delete flag <strong>{deleteTarget}</strong>? Consumers fall back to their disabled
                default. This is audited.
              </>
            }
            confirmLabel="Delete flag"
            busy={busyName === deleteTarget}
            onCancel={() => setDeleteTarget(null)}
            onConfirm={() =>
              void run(deleteTarget, async () => {
                await deleteFlag(apiClient, adminApi.env, deleteTarget);
                setDeleteTarget(null);
              })
            }
          />
        )}
      </Modal>
    </section>
  );
}
