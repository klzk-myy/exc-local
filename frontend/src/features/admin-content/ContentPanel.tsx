/**
 * Content panel — announcement register (all statuses incl. drafts and
 * retracted), create, partial PATCH edit, and retract-as-DELETE (a state
 * transition, never row removal — announcements are disclosure records).
 * Maintenance windows share the Task 5.3.14 seam: list, schedule, and
 * cancel-as-DELETE.
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
  ErrorBox,
  hintTextCls,
  inputCls,
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  textareaCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import {
  cancelMaintWindow,
  createAnnouncement,
  createMaintWindow,
  fetchAnnouncements,
  fetchMaintWindows,
  retractAnnouncement,
  updateAnnouncement,
} from './api';

const CATEGORIES = ['GENERAL', 'MAINTENANCE', 'INCIDENT', 'PRODUCT', 'PROMOTION'];
const STATUSES = ['DRAFT', 'PUBLISHED', 'EXPIRED', 'RETRACTED'];
const SCOPES = ['FULL_VENUE', 'GATEWAY', 'MARKET_DATA', 'SETTLEMENT', 'FUNDING', 'INSTRUMENT'];

export function ContentPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [draft, setDraft] = useState({
    title: '',
    body: '',
    category: 'GENERAL',
    status: 'DRAFT',
    publishAt: '',
    expiresAt: '',
  });
  const [edit, setEdit] = useState<Record<string, string> | null>(null);
  const [win, setWin] = useState({
    title: '',
    scope: 'FULL_VENUE',
    symbols: '',
    startsAt: '',
    endsAt: '',
  });
  const [notice, setNotice] = useState<string | null>(null);

  const anns = useQuery({
    queryKey: ['admin', 'announcements', adminApi.env],
    queryFn: () => fetchAnnouncements(adminApi),
    retry: false,
  });
  const windows = useQuery({
    queryKey: ['admin', 'maint-windows', adminApi.env],
    queryFn: () => fetchMaintWindows(adminApi),
    retry: false,
  });

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin', 'announcements'] });
    void qc.invalidateQueries({ queryKey: ['admin', 'maint-windows'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : String(e));

  const createMut = useMutation({
    mutationFn: () =>
      createAnnouncement(adminApi, {
        title: draft.title,
        body: draft.body,
        category: draft.category,
        status: draft.status,
        publish_at: draft.publishAt,
        ...(draft.expiresAt !== '' ? { expires_at: draft.expiresAt } : {}),
      }),
    onSuccess: () => {
      setNotice('Announcement created.');
      setDraft({ ...draft, title: '', body: '', expiresAt: '' });
      invalidate();
    },
    onError: onErr,
  });
  const patchMut = useMutation({
    mutationFn: () => {
      if (edit === null) return Promise.reject(new Error('No edit target'));
      const { id, ...body } = edit;
      return updateAnnouncement(apiClient, adminApi.env, Number(id), body);
    },
    onSuccess: () => {
      setNotice('Announcement updated.');
      setEdit(null);
      invalidate();
    },
    onError: onErr,
  });
  const retractMut = useMutation({
    mutationFn: (id: number) => retractAnnouncement(apiClient, adminApi.env, id),
    onSuccess: () => {
      setNotice('Announcement RETRACTED (record kept for disclosure audit).');
      invalidate();
    },
    onError: onErr,
  });
  const winCreateMut = useMutation({
    mutationFn: () =>
      createMaintWindow(adminApi, {
        title: win.title,
        scope: win.scope,
        symbols: win.symbols
          .split(',')
          .map((s) => s.trim())
          .filter((s) => s !== ''),
        starts_at: win.startsAt,
        ends_at: win.endsAt,
      }),
    onSuccess: () => {
      setNotice('Maintenance window scheduled.');
      setWin({ ...win, title: '' });
      invalidate();
    },
    onError: onErr,
  });
  const winCancelMut = useMutation({
    mutationFn: (id: number) => cancelMaintWindow(apiClient, adminApi.env, id),
    onSuccess: () => {
      setNotice('Window CANCELLED.');
      invalidate();
    },
    onError: onErr,
  });

  const denied = isAccessDenied(anns.error ?? windows.error);
  if (denied)
    return (
      <AccessDeniedCard detail="Announcements and maintenance windows require a Support Agent role or above." />
    );

  const input = (key: keyof typeof draft, label: string, placeholder = '') => (
    <label className="block">
      <span className="sr-only">{label}</span>
      <input
        aria-label={label}
        className={inputCls}
        placeholder={placeholder || label}
        value={draft[key]}
        onChange={(e) => setDraft({ ...draft, [key]: e.target.value })}
      />
    </label>
  );

  return (
    <section className={cardCls} aria-label="Content">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">
        Announcements &amp; maintenance calendar
      </h2>
      <ErrorBox error={anns.error ?? windows.error} />
      {notice !== null && (
        <p className="mb-2 rounded border border-neutral-700 bg-neutral-950 px-2 py-1 text-xs text-neutral-300">
          {notice}
        </p>
      )}
      {edit !== null && (
        <form
          className="mb-3 grid gap-2 rounded border border-sky-900/60 p-3 sm:grid-cols-4"
          onSubmit={(e) => {
            e.preventDefault();
            patchMut.mutate();
          }}
        >
          <span className="self-center text-xs text-neutral-400">Edit #{edit['id']}</span>
          {(['title', 'status', 'expires_at'] as const).map((k) =>
            k === 'status' ? (
              <label key={k} className="block">
                <span className="sr-only">status</span>
                <select
                  aria-label="Edit status"
                  className={selectCls}
                  value={edit[k] ?? ''}
                  onChange={(e) => setEdit({ ...edit, status: e.target.value })}
                >
                  {STATUSES.map((s) => (
                    <option key={s} value={s}>
                      {s}
                    </option>
                  ))}
                </select>
              </label>
            ) : (
              <label key={k} className="block">
                <span className="sr-only">{k}</span>
                <input
                  aria-label={`Edit ${k}`}
                  className={inputCls}
                  placeholder={k}
                  value={edit[k] ?? ''}
                  onChange={(e) => setEdit({ ...edit, [k]: e.target.value })}
                />
              </label>
            ),
          )}
          <button type="submit" className={btnPrimary} disabled={patchMut.isPending}>
            Save
          </button>
        </form>
      )}
      <table className={tableCls}>
        <thead>
          <tr>
            <th className={thCls}>ID</th>
            <th className={thCls}>Title</th>
            <th className={thCls}>Category</th>
            <th className={thCls}>Status</th>
            <th className={thCls}>Publish</th>
            <th className={thCls}>Expires</th>
            <th className={thCls}></th>
          </tr>
        </thead>
        <tbody>
          {(anns.data ?? []).map((a) => (
            <tr key={a.id}>
              <td className={tdCls}>{a.id}</td>
              <td className={tdCls}>{a.title}</td>
              <td className={tdCls}>{a.category}</td>
              <td className={tdCls}>
                <StatusBadge value={a.status} />
              </td>
              <td className={tdCls}>{a.publishAt.slice(0, 10)}</td>
              <td className={tdCls}>{a.expiresAt?.slice(0, 10) ?? '—'}</td>
              <td className={tdCls}>
                <div className="flex gap-1">
                  <button
                    type="button"
                    className={btnGhost}
                    onClick={() => {
                      setEdit({
                        id: String(a.id),
                        title: a.title,
                        status: a.status,
                        expires_at: a.expiresAt ?? '',
                      });
                    }}
                  >
                    Edit
                  </button>
                  {a.status !== 'RETRACTED' && (
                    <button
                      type="button"
                      className={btnDanger}
                      disabled={retractMut.isPending}
                      onClick={() => retractMut.mutate(a.id)}
                    >
                      Retract
                    </button>
                  )}
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {anns.data?.length === 0 && (
        <p className="py-2 text-center text-xs text-neutral-500">No announcements.</p>
      )}
      <form
        className="mt-3 grid gap-2 sm:grid-cols-3"
        onSubmit={(e) => {
          e.preventDefault();
          createMut.mutate();
        }}
      >
        {input('title', 'Title')}
        {input('publishAt', 'publish_at', 'publish_at (RFC3339)')}
        {input('expiresAt', 'expires_at', 'expires_at (optional RFC3339)')}
        <label className="block">
          <span className="sr-only">Category</span>
          <select
            aria-label="Category"
            className={selectCls}
            value={draft.category}
            onChange={(e) => setDraft({ ...draft, category: e.target.value })}
          >
            {CATEGORIES.map((c) => (
              <option key={c} value={c}>
                {c}
              </option>
            ))}
          </select>
        </label>
        <label className="block">
          <span className="sr-only">Status</span>
          <select
            aria-label="Status"
            className={selectCls}
            value={draft.status}
            onChange={(e) => setDraft({ ...draft, status: e.target.value })}
          >
            {STATUSES.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </label>
        <label className="block sm:col-span-3">
          <span className="sr-only">Body</span>
          <textarea
            aria-label="Announcement body"
            className={textareaCls}
            placeholder="body"
            value={draft.body}
            onChange={(e) => setDraft({ ...draft, body: e.target.value })}
          />
        </label>
        <button
          type="submit"
          className={btnPrimary}
          disabled={createMut.isPending || draft.title === '' || draft.body === ''}
        >
          Create
        </button>
      </form>

      <h3 className="mt-6 mb-2 text-sm font-medium text-neutral-400">Maintenance windows</h3>
      <table className={tableCls}>
        <thead>
          <tr>
            <th className={thCls}>ID</th>
            <th className={thCls}>Title</th>
            <th className={thCls}>Scope</th>
            <th className={thCls}>Symbols</th>
            <th className={thCls}>Status</th>
            <th className={thCls}>Window</th>
            <th className={thCls}></th>
          </tr>
        </thead>
        <tbody>
          {(windows.data ?? []).map((w) => (
            <tr key={w.id}>
              <td className={tdCls}>{w.id}</td>
              <td className={tdCls}>{w.title}</td>
              <td className={tdCls}>{w.scope}</td>
              <td className={tdCls}>{w.symbols.length > 0 ? w.symbols.join(', ') : 'all'}</td>
              <td className={tdCls}>
                <StatusBadge value={w.status} />
              </td>
              <td className={tdCls}>
                {w.startsAt.slice(0, 16)} → {w.endsAt.slice(0, 16)}
              </td>
              <td className={tdCls}>
                {w.status !== 'CANCELLED' && w.status !== 'COMPLETED' && (
                  <button
                    type="button"
                    className={btnDanger}
                    disabled={winCancelMut.isPending}
                    onClick={() => winCancelMut.mutate(w.id)}
                  >
                    Cancel
                  </button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {windows.data?.length === 0 && (
        <p className="py-2 text-center text-xs text-neutral-500">No maintenance windows.</p>
      )}
      <form
        className="mt-3 grid gap-2 sm:grid-cols-5"
        onSubmit={(e) => {
          e.preventDefault();
          winCreateMut.mutate();
        }}
      >
        <label className="block">
          <span className="sr-only">Window title</span>
          <input
            aria-label="Window title"
            className={inputCls}
            placeholder="title"
            value={win.title}
            onChange={(e) => setWin({ ...win, title: e.target.value })}
          />
        </label>
        <label className="block">
          <span className="sr-only">Scope</span>
          <select
            aria-label="Scope"
            className={selectCls}
            value={win.scope}
            onChange={(e) => setWin({ ...win, scope: e.target.value })}
          >
            {SCOPES.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </label>
        <label className="block">
          <span className="sr-only">Symbols</span>
          <input
            aria-label="Symbols"
            className={inputCls}
            placeholder="symbols csv (blank = all)"
            value={win.symbols}
            onChange={(e) => setWin({ ...win, symbols: e.target.value })}
          />
        </label>
        <label className="block">
          <span className="sr-only">starts_at</span>
          <input
            aria-label="starts_at"
            className={inputCls}
            placeholder="starts_at (RFC3339)"
            value={win.startsAt}
            onChange={(e) => setWin({ ...win, startsAt: e.target.value })}
          />
        </label>
        <label className="block">
          <span className="sr-only">ends_at</span>
          <input
            aria-label="ends_at"
            className={inputCls}
            placeholder="ends_at (RFC3339)"
            value={win.endsAt}
            onChange={(e) => setWin({ ...win, endsAt: e.target.value })}
          />
        </label>
        <button
          type="submit"
          className={btnGhost}
          disabled={winCreateMut.isPending || win.title === '' || win.startsAt === ''}
        >
          Schedule
        </button>
      </form>
      <p className={hintTextCls}>
        Deletes are disclosure-preserving state transitions (RETRACTED / CANCELLED) — rows are never
        removed.
      </p>
    </section>
  );
}
