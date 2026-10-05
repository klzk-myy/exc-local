/**
 * Restricted-lists panel (Phase-10.5 Task 10.5.3.5 §3) — dealing
 * restriction administration over member/instrument/counterparty
 * scopes. Create is officer-filed; retire is a destructive op behind a
 * typed confirmation modal (server stamps RETIRED, never hard-deletes).
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
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
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import {
  createRestrictedList,
  fetchRestrictedLists,
  retireRestrictedList,
  type RestrictedListRow,
} from './api';

const SCOPES = ['ALL_STAFF', 'NAMED_ACCOUNTS', 'ROLE'] as const;

export function RestrictedListsPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [notice, setNotice] = useState<string | null>(null);
  const [retireTarget, setRetireTarget] = useState<RestrictedListRow | null>(null);

  const [form, setForm] = useState({
    eventId: '',
    eventType: '',
    instruments: '',
    windowStart: '',
    windowEnd: '',
    scope: 'ALL_STAFF',
    scopeRole: '',
    namedAccounts: '',
    reason: '',
  });

  const list = useQuery({
    queryKey: ['admin-restricted-lists', adminApi.env],
    queryFn: () => fetchRestrictedLists(adminApi),
  });

  const invalidate = () => void qc.invalidateQueries({ queryKey: ['admin-restricted-lists'] });

  const create = useMutation({
    mutationFn: () =>
      createRestrictedList(adminApi, {
        eventId: form.eventId,
        eventType: form.eventType,
        instruments: form.instruments
          .split(',')
          .map((s) => s.trim())
          .filter((s) => s !== ''),
        windowStart: form.windowStart,
        windowEnd: form.windowEnd,
        scope: form.scope,
        scopeRole: form.scopeRole === '' ? undefined : form.scopeRole,
        namedAccounts: form.namedAccounts
          .split(',')
          .map((s) => Number(s.trim()))
          .filter((n) => n > 0),
        reason: form.reason,
      }),
    onSuccess: () => {
      setNotice('Restricted list created.');
      invalidate();
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Create failed'),
  });

  const retire = useMutation({
    mutationFn: (id: number) => retireRestrictedList(apiClient, adminApi.env, id),
    onSuccess: () => {
      setRetireTarget(null);
      setNotice('Restricted list retired.');
      invalidate();
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Retire failed'),
  });

  if (list.error !== null && isAccessDenied(list.error)) {
    return <AccessDeniedCard />;
  }

  const createValid =
    form.eventId !== '' &&
    form.eventType !== '' &&
    form.windowStart !== '' &&
    form.windowEnd !== '' &&
    form.reason !== '';

  return (
    <section className={cardCls} aria-label="Restricted lists">
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-sm font-semibold">Restricted lists</h2>
        <button
          type="button"
          className={btnGhost}
          onClick={() => void list.refetch()}
          disabled={list.isFetching}
        >
          Refresh
        </button>
      </div>
      <p className={hintTextCls}>
        Instruments empty = venue-wide. Scope: all staff, a staff role, or named accounts.
      </p>

      {list.error !== null ? <ErrorBox error={list.error} /> : null}
      {list.data?.length === 0 ? (
        <p className="mt-2 text-sm text-neutral-500">No restricted lists.</p>
      ) : null}
      {list.data !== undefined && list.data.length > 0 ? (
        <div className="mt-2 max-h-64 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Event</th>
                <th className={thCls}>Type</th>
                <th className={thCls}>Scope</th>
                <th className={thCls}>Instruments</th>
                <th className={thCls}>Window</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {list.data.map((r) => (
                <tr key={r.id}>
                  <td className={tdCls}>{r.id}</td>
                  <td className={tdCls}>{r.eventId}</td>
                  <td className={tdCls}>{r.eventType}</td>
                  <td className={tdCls}>
                    {r.scope}
                    {r.scopeRole !== undefined ? `:${r.scopeRole}` : ''}
                    {r.namedAccounts.length > 0 ? ` (${r.namedAccounts.length} accts)` : ''}
                  </td>
                  <td className={tdCls}>
                    {r.instruments.length > 0 ? r.instruments.join(', ') : 'venue-wide'}
                  </td>
                  <td className={tdCls}>
                    {r.windowStart} → {r.windowEnd}
                  </td>
                  <td className={tdCls}>
                    <StatusBadge value={r.status || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>
                    {r.status === 'ACTIVE' ? (
                      <button
                        type="button"
                        className={btnDanger}
                        onClick={() => setRetireTarget(r)}
                      >
                        Retire…
                      </button>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      <form
        aria-label="Create restricted list"
        className="mt-4 space-y-2 border-t border-neutral-800 pt-3"
        onSubmit={(e) => {
          e.preventDefault();
          if (createValid) create.mutate();
        }}
      >
        <p className={hintTextCls}>New restriction</p>
        <div className="grid grid-cols-1 gap-2 md:grid-cols-3">
          <div>
            <label className={labelCls} htmlFor="rl-event">
              Event id
            </label>
            <input
              id="rl-event"
              className={inputCls}
              value={form.eventId}
              onChange={(e) => {
                setForm({ ...form, eventId: e.target.value });
              }}
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="rl-type">
              Event type
            </label>
            <input
              id="rl-type"
              className={inputCls}
              placeholder="EARNINGS / CORP_ACTION…"
              value={form.eventType}
              onChange={(e) => {
                setForm({ ...form, eventType: e.target.value });
              }}
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="rl-scope">
              Scope
            </label>
            <select
              id="rl-scope"
              className={selectCls}
              value={form.scope}
              onChange={(e) => {
                setForm({ ...form, scope: e.target.value });
              }}
            >
              {SCOPES.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
          </div>
          {form.scope === 'ROLE' ? (
            <div>
              <label className={labelCls} htmlFor="rl-role">
                Scope role
              </label>
              <input
                id="rl-role"
                className={inputCls}
                value={form.scopeRole}
                onChange={(e) => {
                  setForm({ ...form, scopeRole: e.target.value });
                }}
              />
            </div>
          ) : null}
          {form.scope === 'NAMED_ACCOUNTS' ? (
            <div>
              <label className={labelCls} htmlFor="rl-accts">
                Account ids (csv)
              </label>
              <input
                id="rl-accts"
                className={inputCls}
                value={form.namedAccounts}
                onChange={(e) => {
                  setForm({ ...form, namedAccounts: e.target.value });
                }}
              />
            </div>
          ) : null}
          <div>
            <label className={labelCls} htmlFor="rl-inst">
              Instruments (csv)
            </label>
            <input
              id="rl-inst"
              className={inputCls}
              placeholder="EURUSD, GBPUSD"
              value={form.instruments}
              onChange={(e) => {
                setForm({ ...form, instruments: e.target.value });
              }}
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="rl-start">
              Window start (RFC3339)
            </label>
            <input
              id="rl-start"
              className={inputCls}
              value={form.windowStart}
              onChange={(e) => {
                setForm({ ...form, windowStart: e.target.value });
              }}
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="rl-end">
              Window end (RFC3339)
            </label>
            <input
              id="rl-end"
              className={inputCls}
              value={form.windowEnd}
              onChange={(e) => {
                setForm({ ...form, windowEnd: e.target.value });
              }}
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="rl-reason">
              Reason
            </label>
            <input
              id="rl-reason"
              className={inputCls}
              value={form.reason}
              onChange={(e) => {
                setForm({ ...form, reason: e.target.value });
              }}
            />
          </div>
        </div>
        <button type="submit" className={btnPrimary} disabled={create.isPending || !createValid}>
          Create restriction
        </button>
        {notice !== null ? <p className="text-sm">{notice}</p> : null}
      </form>

      <Modal
        open={retireTarget !== null}
        title="Retire restricted list"
        onClose={() => setRetireTarget(null)}
      >
        <ConfirmAction
          message={`Retire restricted list #${retireTarget?.id ?? ''} (${retireTarget?.eventId ?? ''})? The server stamps it RETIRED — staff dealing resumes inside the window.`}
          confirmLabel="Retire"
          busy={retire.isPending}
          onConfirm={() => {
            if (retireTarget !== null) retire.mutate(retireTarget.id);
          }}
          onCancel={() => setRetireTarget(null)}
        />
      </Modal>
    </section>
  );
}
