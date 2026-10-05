/**
 * MM-program admin panel (Phase-10.5 Task 10.5.3.12 §1) — enrollment
 * register (account/status filters), quota terms (min quote size /
 * max spread / presence %, MMP thresholds, rebate bps, OTR allowance),
 * suspend/resume + the §24 #139 MMP lockout reset, per-program
 * compliance sampling and rebate accruals, plus the monthly GL
 * rebate sweep (partial failures reported, not hidden).
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
  JsonRows,
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import {
  enrollMMProgram,
  fetchMMCompliance,
  fetchMMPrograms,
  fetchMMRebates,
  fetchMmProgram,
  mmProgramAction,
  postMMRebates,
  updateMMProgram,
  type MMComplianceRow,
  type MMProgram,
  type MMRebate,
} from './api';

export function MMProgramsPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [accountFilter, setAccountFilter] = useState('');
  const [statusFilter, setStatusFilter] = useState('');
  const [detail, setDetail] = useState<MMProgram | null>(null);
  const [compliance, setCompliance] = useState<MMComplianceRow[] | null>(null);
  const [rebates, setRebates] = useState<MMRebate[] | null>(null);
  const [editing, setEditing] = useState<number | null>(null);
  const [form, setForm] = useState({
    accountId: '',
    instrumentId: '',
    minQuoteSize: '',
    maxSpreadBps: '',
    presencePct: '',
    mmpMaxFills: '',
    mmpWindowMs: '',
    rebateBps: '',
  });
  const [notice, setNotice] = useState<string | null>(null);

  const list = useQuery({
    queryKey: ['admin-mm-programs', accountFilter, statusFilter],
    queryFn: () =>
      fetchMMPrograms(adminApi, {
        accountId: Number(accountFilter) || undefined,
        status: statusFilter === '' ? undefined : statusFilter,
      }),
  });
  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-mm-programs'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const action = useMutation({
    mutationFn: (v: { id: number; verb: 'suspend' | 'resume' | 'mmp-reset'; reason?: string }) =>
      mmProgramAction(adminApi, v.id, v.verb, v.reason),
    onSuccess: (_d, v) => {
      setNotice(
        v.verb === 'mmp-reset'
          ? `MMP lockout + fill window cleared for program #${v.id}.`
          : `Program #${v.id} ${v.verb === 'suspend' ? 'suspended' : 'resumed'}.`,
      );
      invalidate();
    },
    onError: onErr,
  });
  const detailRecord = useQuery({
    queryKey: ['admin-mm-programs', 'detail', detail?.id],
    queryFn: () => fetchMmProgram(adminApi, detail?.id ?? 0),
    enabled: detail !== null,
  });
  const loadCompliance = useMutation({
    mutationFn: (id: number) => fetchMMCompliance(adminApi, id, {}),
    onSuccess: (rows, id) => {
      setCompliance(rows);
      setNotice(`Compliance window loaded for program #${id} (default 7d).`);
    },
    onError: onErr,
  });
  const loadRebates = useMutation({
    mutationFn: (id: number) => fetchMMRebates(adminApi, id),
    onSuccess: (rows, id) => {
      setRebates(rows);
      setNotice(`Rebate accruals loaded for program #${id}.`);
    },
    onError: onErr,
  });
  const enroll = useMutation({
    mutationFn: () =>
      editing !== null
        ? updateMMProgram(apiClient, adminApi.env, editing, {
            accountId: Number(form.accountId),
            instrumentId: Number(form.instrumentId) || undefined,
            minQuoteSize: form.minQuoteSize,
            maxSpreadBps: form.maxSpreadBps,
            presencePct: form.presencePct,
            mmpMaxFills: Number(form.mmpMaxFills),
            mmpWindowMs: Number(form.mmpWindowMs),
            rebateBps: form.rebateBps,
          })
        : enrollMMProgram(adminApi, {
            accountId: Number(form.accountId),
            instrumentId: Number(form.instrumentId) || undefined,
            minQuoteSize: form.minQuoteSize,
            maxSpreadBps: form.maxSpreadBps,
            presencePct: form.presencePct,
            mmpMaxFills: Number(form.mmpMaxFills),
            mmpWindowMs: Number(form.mmpWindowMs),
            rebateBps: form.rebateBps,
          }),
    onSuccess: (p) => {
      setNotice(
        editing !== null
          ? `Program #${p.id} terms updated.`
          : `Program #${p.id} enrolled (${p.status}).`,
      );
      setEditing(null);
      invalidate();
    },
    onError: onErr,
  });
  const sweep = useMutation({
    mutationFn: () => postMMRebates(adminApi),
    onSuccess: (r) => {
      setNotice(
        r.partialError !== undefined
          ? `Sweep partial: ${r.posted} journals posted — first failure: ${r.partialError}`
          : `Sweep complete: ${r.posted} rebate journals posted to GL.`,
      );
    },
    onError: onErr,
  });

  if (list.error !== null && isAccessDenied(list.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="MM programs">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Market-maker programs</h2>
        <div className="flex items-end gap-2">
          <input
            aria-label="Account filter"
            className={inputCls}
            placeholder="account_id"
            value={accountFilter}
            onChange={(e) => {
              setAccountFilter(e.target.value);
            }}
          />
          <select
            aria-label="Program status filter"
            className={selectCls}
            value={statusFilter}
            onChange={(e) => {
              setStatusFilter(e.target.value);
            }}
          >
            <option value="">ALL</option>
            <option value="ACTIVE">ACTIVE</option>
            <option value="SUSPENDED">SUSPENDED</option>
          </select>
          <button
            type="button"
            className={btnGhost}
            disabled={sweep.isPending}
            onClick={() => {
              sweep.mutate();
            }}
          >
            Post monthly rebates
          </button>
        </div>
      </div>
      <p className={hintTextCls}>
        Suspension freezes quoting immediately; MMP reset clears the lockout + fill window (§24
        #139). The monthly sweep posts balanced journals to GL.
      </p>
      {list.error !== null ? <ErrorBox error={list.error} /> : null}
      {list.data?.length === 0 ? <p className="text-sm text-neutral-500">No programs.</p> : null}
      {list.data !== undefined && list.data.length > 0 ? (
        <div className="max-h-60 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Account</th>
                <th className={thCls}>Instrument</th>
                <th className={thCls}>Min qty</th>
                <th className={thCls}>Spread bps</th>
                <th className={thCls}>Presence %</th>
                <th className={thCls}>MMP</th>
                <th className={thCls}>Rebate</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {list.data.map((p) => (
                <tr key={p.id}>
                  <td className={tdCls}>{p.id}</td>
                  <td className={tdCls}>{p.accountId}</td>
                  <td className={tdCls}>{p.symbol ?? p.instrumentId ?? 'ALL'}</td>
                  <td className={tdCls}>{p.minQuoteSize}</td>
                  <td className={tdCls}>{p.maxSpreadBps}</td>
                  <td className={tdCls}>{p.presencePct}</td>
                  <td className={tdCls}>
                    {p.mmpMaxFills}/{p.mmpWindowMs}ms
                  </td>
                  <td className={tdCls}>{p.rebateBps}bps</td>
                  <td className={tdCls}>
                    <StatusBadge value={p.status || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>
                    <div className="flex flex-wrap gap-1">
                      <button
                        type="button"
                        className={btnGhost}
                        onClick={() => {
                          setDetail(p);
                          setCompliance(null);
                          setRebates(null);
                        }}
                      >
                        Detail
                      </button>
                      <button
                        type="button"
                        className={btnGhost}
                        onClick={() => {
                          setEditing(p.id);
                          setForm({
                            accountId: String(p.accountId),
                            instrumentId:
                              p.instrumentId !== undefined ? String(p.instrumentId) : '',
                            minQuoteSize: p.minQuoteSize,
                            maxSpreadBps: p.maxSpreadBps,
                            presencePct: p.presencePct,
                            mmpMaxFills: String(p.mmpMaxFills),
                            mmpWindowMs: String(p.mmpWindowMs),
                            rebateBps: p.rebateBps,
                          });
                        }}
                      >
                        Edit
                      </button>
                      {p.status === 'SUSPENDED' ? (
                        <button
                          type="button"
                          className={btnPrimary}
                          disabled={action.isPending}
                          onClick={() => {
                            action.mutate({ id: p.id, verb: 'resume' });
                          }}
                        >
                          Resume
                        </button>
                      ) : (
                        <button
                          type="button"
                          className={btnDanger}
                          disabled={action.isPending}
                          onClick={() => {
                            action.mutate({ id: p.id, verb: 'suspend' });
                          }}
                        >
                          Suspend
                        </button>
                      )}
                      <button
                        type="button"
                        className={btnGhost}
                        disabled={action.isPending}
                        onClick={() => {
                          action.mutate({ id: p.id, verb: 'mmp-reset' });
                        }}
                      >
                        MMP reset
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      <form
        aria-label="Enroll MM program"
        className="mt-3 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(form.accountId) > 0 && form.minQuoteSize !== '') enroll.mutate();
        }}
      >
        {(
          [
            ['accountId', 'account_id'],
            ['instrumentId', 'instrument_id (blank=all)'],
            ['minQuoteSize', 'min_quote_size'],
            ['maxSpreadBps', 'max_spread_bps'],
            ['presencePct', 'presence_pct'],
            ['mmpMaxFills', 'mmp_max_fills'],
            ['mmpWindowMs', 'mmp_window_ms'],
            ['rebateBps', 'rebate_bps'],
          ] as const
        ).map(([k, ph]) => (
          <input
            key={k}
            aria-label={`Program ${k}`}
            className={inputCls}
            placeholder={ph}
            value={form[k]}
            onChange={(e) => {
              setForm({ ...form, [k]: e.target.value });
            }}
          />
        ))}
        <button type="submit" className={btnPrimary} disabled={enroll.isPending}>
          {editing !== null ? `Update #${editing}` : 'Enroll'}
        </button>
        {editing !== null ? (
          <button
            type="button"
            className={btnGhost}
            onClick={() => {
              setEditing(null);
            }}
          >
            Cancel
          </button>
        ) : null}
      </form>

      {detail !== null ? (
        <div className="mt-2 rounded border border-neutral-700 p-2" aria-label="Program detail">
          <p className="mb-1 text-xs font-semibold">
            Program #{detail.id} — OTR allowance {detail.otrAllowance ?? 'none'}
          </p>
          {detailRecord.isPending ? (
            <p className="text-xs text-neutral-500">Loading record…</p>
          ) : detailRecord.isError ? (
            <ErrorBox error={detailRecord.error} />
          ) : (
            <div className="mb-2">
              <JsonRows rows={[detailRecord.data]} />
            </div>
          )}
          <div className="flex gap-2">
            <button
              type="button"
              className={btnGhost}
              disabled={loadCompliance.isPending}
              onClick={() => {
                loadCompliance.mutate(detail.id);
              }}
            >
              Load compliance
            </button>
            <button
              type="button"
              className={btnGhost}
              disabled={loadRebates.isPending}
              onClick={() => {
                loadRebates.mutate(detail.id);
              }}
            >
              Load rebates
            </button>
          </div>
          {compliance !== null ? (
            <table className={`${tableCls} mt-2`}>
              <thead>
                <tr>
                  <th className={thCls}>Day</th>
                  <th className={thCls}>Samples</th>
                  <th className={thCls}>Compliant</th>
                  <th className={thCls}>Presence</th>
                  <th className={thCls}>Breach</th>
                </tr>
              </thead>
              <tbody>
                {compliance.map((c) => (
                  <tr key={c.day}>
                    <td className={tdCls}>{c.day.slice(0, 10)}</td>
                    <td className={tdCls}>{c.samplesTotal}</td>
                    <td className={tdCls}>{c.samplesCompliant}</td>
                    <td className={tdCls}>{c.presencePct ?? '—'}</td>
                    <td className={tdCls}>
                      {c.breach ? `BREACH — ${c.breachReason ?? ''}` : 'ok'}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : null}
          {rebates !== null ? (
            <table className={`${tableCls} mt-2`}>
              <thead>
                <tr>
                  <th className={thCls}>Day</th>
                  <th className={thCls}>Currency</th>
                  <th className={thCls}>Amount</th>
                  <th className={thCls}>GL journal</th>
                </tr>
              </thead>
              <tbody>
                {rebates.map((r) => (
                  <tr key={r.id}>
                    <td className={tdCls}>{r.day.slice(0, 10)}</td>
                    <td className={tdCls}>{r.currency}</td>
                    <td className={tdCls}>{r.amount}</td>
                    <td className={tdCls}>{r.postedJournalId ?? 'accrued'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : null}
        </div>
      ) : null}
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
