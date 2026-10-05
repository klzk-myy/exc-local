/**
 * Market schedule + auction calendar panel (Phase-10.5 Task 10.5.3.11
 * §1c/§2) — the 24/5 market-schedule document (open/close/pre-open
 * UTC + published version) with holiday-override CRUD, plus the
 * per-symbol auction calendar: GET the entry set, PUT a full-replace
 * (dual-control — 202 queued, never applied optimistically).
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
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import {
  createScheduleOverride,
  deleteScheduleOverride,
  fetchAuctionCalendar,
  fetchMarketSchedule,
  fetchScheduleOverrides,
  putAuctionCalendar,
  updateScheduleOverride,
  type CalendarEntry,
} from './api';

export function SchedulePanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [override, setOverride] = useState({
    date: '',
    closed: true,
    open: '',
    close: '',
    reason: '',
  });
  const [editingId, setEditingId] = useState<number | null>(null);
  const [calSymbol, setCalSymbol] = useState('EURUSD');
  const [loadedSymbol, setLoadedSymbol] = useState<string | null>(null);
  const [entries, setEntries] = useState<CalendarEntry[] | null>(null);
  const [entriesText, setEntriesText] = useState('');
  const [calReason, setCalReason] = useState('');
  const [notice, setNotice] = useState<string | null>(null);

  const [includeExpired, setIncludeExpired] = useState(false);

  const schedule = useQuery({
    queryKey: ['admin-market-schedule'],
    queryFn: () => fetchMarketSchedule(adminApi),
  });
  // The dedicated overrides register includes expired rows the base
  // schedule document trims — toggled on demand, not polled.
  const overridesRegister = useQuery({
    queryKey: ['admin-market-schedule', 'overrides'],
    queryFn: () => fetchScheduleOverrides(adminApi),
    enabled: includeExpired,
  });
  const overrideRows =
    includeExpired && overridesRegister.data !== undefined
      ? overridesRegister.data
      : (schedule.data?.overrides ?? []);
  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-market-schedule'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const loadCal = useMutation({
    mutationFn: (symbol: string) => fetchAuctionCalendar(adminApi, symbol),
    onSuccess: (rows, symbol) => {
      setLoadedSymbol(symbol);
      setEntries(rows);
      setEntriesText(JSON.stringify(rows, null, 2));
    },
    onError: onErr,
  });
  const saveCal = useMutation({
    mutationFn: () =>
      putAuctionCalendar(
        apiClient,
        adminApi.env,
        loadedSymbol ?? calSymbol,
        JSON.parse(entriesText) as CalendarEntry[],
        calReason,
      ),
    onSuccess: (p) => {
      setNotice(
        `Calendar replace queued for four-eyes — request #${p.dualControlId}; live calendar unchanged until approval.`,
      );
    },
    onError: onErr,
  });
  const createOvr = useMutation({
    mutationFn: () =>
      editingId !== null
        ? updateScheduleOverride(apiClient, adminApi.env, editingId, {
            date: override.date,
            closed: override.closed,
            open: override.open === '' ? undefined : override.open,
            close: override.close === '' ? undefined : override.close,
            reason: override.reason,
          })
        : createScheduleOverride(adminApi, {
            date: override.date,
            closed: override.closed,
            open: override.open === '' ? undefined : override.open,
            close: override.close === '' ? undefined : override.close,
            reason: override.reason,
          }),
    onSuccess: () => {
      setNotice(editingId !== null ? 'Override updated.' : 'Override recorded.');
      setEditingId(null);
      invalidate();
    },
    onError: onErr,
  });
  const deleteOvr = useMutation({
    mutationFn: (id: number) => deleteScheduleOverride(apiClient, adminApi.env, id),
    onSuccess: () => {
      setNotice('Override removed.');
      invalidate();
    },
    onError: onErr,
  });

  if (schedule.error !== null && isAccessDenied(schedule.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Market schedule">
      <h2 className="mb-2 text-sm font-semibold">Market schedule (24/5)</h2>
      {schedule.error !== null ? <ErrorBox error={schedule.error} /> : null}
      {schedule.data !== undefined ? (
        <p className={hintTextCls}>
          open {schedule.data.openUtc} → close {schedule.data.closeUtc} UTC · pre-open{' '}
          {schedule.data.preOpenUtc} · version {schedule.data.version} · published{' '}
          {schedule.data.publishedAt.slice(0, 16)}
        </p>
      ) : null}
      <label className="mt-1 flex items-center gap-1 text-xs text-neutral-400">
        <input
          type="checkbox"
          checked={includeExpired}
          onChange={(e) => {
            setIncludeExpired(e.target.checked);
          }}
        />
        include expired overrides (dedicated register)
      </label>
      {overridesRegister.isError ? <ErrorBox error={overridesRegister.error} /> : null}
      {overrideRows.length > 0 ? (
        <table className={`${tableCls} mt-2`}>
          <thead>
            <tr>
              <th className={thCls}>Date</th>
              <th className={thCls}>Closed</th>
              <th className={thCls}>Open</th>
              <th className={thCls}>Close</th>
              <th className={thCls}>Reason</th>
              <th className={thCls}>
                <span className="sr-only">Delete</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {overrideRows.map((o) => (
              <tr key={o.id}>
                <td className={tdCls}>{o.date}</td>
                <td className={tdCls}>{o.closed ? 'full-day' : 'partial'}</td>
                <td className={tdCls}>{o.open ?? '—'}</td>
                <td className={tdCls}>{o.close ?? '—'}</td>
                <td className={tdCls}>{o.reason}</td>
                <td className={tdCls}>
                  <div className="flex gap-1">
                    <button
                      type="button"
                      className={btnGhost}
                      onClick={() => {
                        setEditingId(o.id);
                        setOverride({
                          date: o.date,
                          closed: o.closed,
                          open: o.open ?? '',
                          close: o.close ?? '',
                          reason: o.reason,
                        });
                      }}
                    >
                      Edit
                    </button>
                    <button
                      type="button"
                      className={btnDanger}
                      disabled={deleteOvr.isPending}
                      onClick={() => {
                        deleteOvr.mutate(o.id);
                      }}
                    >
                      Delete
                    </button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
      <form
        aria-label="Create schedule override"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (override.date !== '' && override.reason !== '') createOvr.mutate();
        }}
      >
        <input
          aria-label="Override date"
          className={inputCls}
          type="date"
          value={override.date}
          onChange={(e) => {
            setOverride({ ...override, date: e.target.value });
          }}
        />
        <label className="flex items-center gap-1 text-sm">
          <input
            type="checkbox"
            checked={override.closed}
            onChange={(e) => {
              setOverride({ ...override, closed: e.target.checked });
            }}
          />
          full-day closed
        </label>
        <input
          aria-label="Override open"
          className={inputCls}
          placeholder="open HH:MM (partial)"
          value={override.open}
          onChange={(e) => {
            setOverride({ ...override, open: e.target.value });
          }}
        />
        <input
          aria-label="Override close"
          className={inputCls}
          placeholder="close HH:MM (partial)"
          value={override.close}
          onChange={(e) => {
            setOverride({ ...override, close: e.target.value });
          }}
        />
        <input
          aria-label="Override reason"
          className={inputCls}
          placeholder="reason (holiday)"
          value={override.reason}
          onChange={(e) => {
            setOverride({ ...override, reason: e.target.value });
          }}
        />
        <button type="submit" className={btnPrimary} disabled={createOvr.isPending}>
          {editingId !== null ? `Update #${editingId}` : 'Add override'}
        </button>
        {editingId !== null ? (
          <button
            type="button"
            className={btnGhost}
            onClick={() => {
              setEditingId(null);
              setOverride({ date: '', closed: true, open: '', close: '', reason: '' });
            }}
          >
            Cancel
          </button>
        ) : null}
      </form>

      <div className="mt-4 border-t border-neutral-800 pt-3">
        <h3 className="mb-1 text-sm font-semibold">Auction calendar</h3>
        <p className={hintTextCls}>
          PUT is a full-replace under dual control — the request queues; the live calendar does not
          change until the second signature.
        </p>
        <div className="flex flex-wrap items-end gap-2">
          <input
            aria-label="Calendar symbol"
            className={inputCls}
            placeholder="symbol"
            value={calSymbol}
            onChange={(e) => {
              setCalSymbol(e.target.value.toUpperCase());
            }}
          />
          <button
            type="button"
            className={btnGhost}
            disabled={loadCal.isPending || calSymbol === ''}
            onClick={() => {
              loadCal.mutate(calSymbol);
            }}
          >
            Load entries
          </button>
        </div>
        {entries !== null ? (
          <>
            <p className={`${hintTextCls} mt-2`}>
              {entries.length} entries for {loadedSymbol}
              {entries.map((e) => ` · ${e.auctionType} ${e.triggerTime}`).join('')}
            </p>
            <textarea
              aria-label="Calendar entries JSON"
              className={`${inputCls} mt-1 h-40 w-full font-mono`}
              value={entriesText}
              onChange={(e) => {
                setEntriesText(e.target.value);
              }}
            />
            <div className="mt-1 flex flex-wrap items-end gap-2">
              <input
                aria-label="Calendar replace reason"
                className={inputCls}
                placeholder="reason"
                value={calReason}
                onChange={(e) => {
                  setCalReason(e.target.value);
                }}
              />
              <button
                type="button"
                className={btnPrimary}
                disabled={saveCal.isPending || calReason === ''}
                onClick={() => {
                  saveCal.mutate();
                }}
              >
                Replace calendar (4-eyes)
              </button>
            </div>
          </>
        ) : null}
      </div>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
