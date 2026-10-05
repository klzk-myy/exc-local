/**
 * SAR & CTR panel (Phase-10.5 Task 10.5.3.6 §2) — the SAR filing
 * lifecycle: manual draft → UNDER_REVIEW → APPROVED → FILED with
 * four-eyes on approve/file (the server returns SAR_DUAL_CONTROL_-
 * REQUIRED on same-principal transitions; filing_ref is mandatory and
 * immutable after). CTR register renders below as the FinCEN trigger
 * view.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnDanger,
  btnPrimary,
  cardCls,
  ErrorBox,
  hintTextCls,
  inputCls,
  labelCls,
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import { createSarDraft, fetchCtrs, fetchSars, sarAction, type SarReport } from './api';

const STATUS_FILTERS = ['', 'DRAFT', 'UNDER_REVIEW', 'APPROVED', 'FILED', 'REJECTED'] as const;

export function SarCtrPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [statusFilter, setStatusFilter] = useState<string>('');
  const [selected, setSelected] = useState<SarReport | null>(null);
  const [note, setNote] = useState('');
  const [filingRef, setFilingRef] = useState('');
  const [rejectReason, setRejectReason] = useState('');
  const [notice, setNotice] = useState<string | null>(null);
  const [draft, setDraft] = useState({ subjectRef: '', description: '', sourceRef: '' });

  const sars = useQuery({
    queryKey: ['admin-sar', statusFilter],
    queryFn: () => fetchSars(adminApi, statusFilter),
  });
  const ctrs = useQuery({ queryKey: ['admin-ctr'], queryFn: () => fetchCtrs(adminApi) });

  const invalidate = () => void qc.invalidateQueries({ queryKey: ['admin-sar'] });
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const act = useMutation({
    mutationFn: (input: {
      action: 'review' | 'approve' | 'file' | 'reject';
      body: Record<string, string>;
    }) => sarAction(adminApi, selected?.id ?? 0, input.action, input.body),
    onSuccess: (sar) => {
      setNotice(`SAR #${sar.id} → ${sar.status}`);
      setSelected(sar);
      invalidate();
    },
    onError: onErr,
  });

  const create = useMutation({
    mutationFn: () =>
      createSarDraft(adminApi, {
        subjectRef: draft.subjectRef,
        description: draft.description,
        sourceRef: draft.sourceRef,
      }),
    onSuccess: ({ sar, created }) => {
      setNotice(created ? `SAR #${sar.id} drafted.` : `SAR #${sar.id} already exists (dedup).`);
      invalidate();
    },
    onError: onErr,
  });

  if (sars.error !== null && isAccessDenied(sars.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="SAR and CTR">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">SAR lifecycle</h2>
        <div>
          <label className={labelCls} htmlFor="sar-status">
            Status
          </label>
          <select
            id="sar-status"
            className={selectCls}
            value={statusFilter}
            onChange={(e) => {
              setStatusFilter(e.target.value);
            }}
          >
            {STATUS_FILTERS.map((s) => (
              <option key={s} value={s}>
                {s === '' ? 'All' : s}
              </option>
            ))}
          </select>
        </div>
      </div>
      <p className={hintTextCls}>
        DRAFT → UNDER_REVIEW → APPROVED → FILED. Approve/file are four-eyes transitions — the server
        rejects same-principal calls.
      </p>

      {sars.error !== null ? <ErrorBox error={sars.error} /> : null}
      {sars.data?.length === 0 ? (
        <p className="mt-2 text-sm text-neutral-500">No SAR reports.</p>
      ) : null}
      {sars.data !== undefined && sars.data.length > 0 ? (
        <div className="mt-2 max-h-56 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Trigger</th>
                <th className={thCls}>Account</th>
                <th className={thCls}>Subject</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Deadline</th>
                <th className={thCls}>Filing ref</th>
              </tr>
            </thead>
            <tbody>
              {sars.data.map((r) => (
                <tr
                  key={r.id}
                  className="cursor-pointer"
                  onClick={() => {
                    setSelected(r);
                    setNotice(null);
                  }}
                >
                  <td className={tdCls}>{r.id}</td>
                  <td className={tdCls}>{r.triggerType}</td>
                  <td className={tdCls}>{r.accountId ?? '—'}</td>
                  <td className={tdCls}>{r.subjectRef ?? '—'}</td>
                  <td className={tdCls}>
                    <StatusBadge value={r.status || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>{r.filingDeadline}</td>
                  <td className={tdCls}>{r.filingRef ?? '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      {selected !== null ? (
        <div
          aria-label="SAR actions"
          className="mt-3 space-y-2 rounded border border-neutral-700 p-3"
        >
          <p className="text-sm">
            SAR <strong>#{selected.id}</strong> <StatusBadge value={selected.status || 'UNKNOWN'} />{' '}
            — {selected.description}
          </p>
          <div className="flex flex-wrap items-end gap-2">
            <div>
              <label className={labelCls} htmlFor="sar-note">
                Note
              </label>
              <input
                id="sar-note"
                className={inputCls}
                value={note}
                onChange={(e) => {
                  setNote(e.target.value);
                }}
              />
            </div>
            {selected.status === 'DRAFT' ? (
              <button
                type="button"
                className={btnPrimary}
                disabled={act.isPending}
                onClick={() => act.mutate({ action: 'review', body: { note } })}
              >
                Start review
              </button>
            ) : null}
            {selected.status === 'UNDER_REVIEW' ? (
              <button
                type="button"
                className={btnPrimary}
                disabled={act.isPending}
                onClick={() => act.mutate({ action: 'approve', body: { note } })}
              >
                Approve (4-eyes)
              </button>
            ) : null}
            {selected.status === 'APPROVED' ? (
              <>
                <input
                  aria-label="Filing ref"
                  className={inputCls}
                  placeholder="FinCEN filing_ref"
                  value={filingRef}
                  onChange={(e) => {
                    setFilingRef(e.target.value);
                  }}
                />
                <button
                  type="button"
                  className={btnPrimary}
                  disabled={act.isPending || filingRef === ''}
                  onClick={() => act.mutate({ action: 'file', body: { filing_ref: filingRef } })}
                >
                  Mark filed
                </button>
              </>
            ) : null}
            {selected.status === 'DRAFT' || selected.status === 'UNDER_REVIEW' ? (
              <>
                <input
                  aria-label="Reject reason"
                  className={inputCls}
                  placeholder="reject reason"
                  value={rejectReason}
                  onChange={(e) => {
                    setRejectReason(e.target.value);
                  }}
                />
                <button
                  type="button"
                  className={btnDanger}
                  disabled={act.isPending || rejectReason === ''}
                  onClick={() => act.mutate({ action: 'reject', body: { reason: rejectReason } })}
                >
                  Reject
                </button>
              </>
            ) : null}
          </div>
        </div>
      ) : null}

      <form
        aria-label="Create SAR draft"
        className="mt-3 space-y-2 border-t border-neutral-800 pt-3"
        onSubmit={(e) => {
          e.preventDefault();
          if (draft.description !== '' && draft.sourceRef !== '') create.mutate();
        }}
      >
        <p className={hintTextCls}>Manual draft</p>
        <div className="grid grid-cols-1 gap-2 md:grid-cols-3">
          <input
            aria-label="Subject ref"
            className={inputCls}
            placeholder="subject_ref"
            value={draft.subjectRef}
            onChange={(e) => {
              setDraft({ ...draft, subjectRef: e.target.value });
            }}
          />
          <input
            aria-label="Source ref"
            className={inputCls}
            placeholder="source_ref (dedup key)"
            value={draft.sourceRef}
            onChange={(e) => {
              setDraft({ ...draft, sourceRef: e.target.value });
            }}
          />
          <input
            aria-label="Description"
            className={inputCls}
            placeholder="description"
            value={draft.description}
            onChange={(e) => {
              setDraft({ ...draft, description: e.target.value });
            }}
          />
        </div>
        <button type="submit" className={btnPrimary} disabled={create.isPending}>
          Draft SAR
        </button>
      </form>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}

      <div className="mt-4 border-t border-neutral-800 pt-3">
        <p className={hintTextCls}>CTR register (FinCEN cash triggers)</p>
        {ctrs.data !== undefined && ctrs.data.length > 0 ? (
          <div className="mt-1 max-h-40 overflow-y-auto">
            <table className={tableCls}>
              <thead>
                <tr>
                  <th className={thCls}>ID</th>
                  <th className={thCls}>Account</th>
                  <th className={thCls}>Date</th>
                  <th className={thCls}>Txns</th>
                  <th className={thCls}>Total USD</th>
                  <th className={thCls}>Status</th>
                  <th className={thCls}>SAR</th>
                </tr>
              </thead>
              <tbody>
                {ctrs.data.map((c) => (
                  <tr key={c.id}>
                    <td className={tdCls}>{c.id}</td>
                    <td className={tdCls}>{c.accountId}</td>
                    <td className={tdCls}>{c.businessDate}</td>
                    <td className={tdCls}>{c.txnCount}</td>
                    <td className={tdCls}>{c.totalUsd ?? '—'}</td>
                    <td className={tdCls}>
                      <StatusBadge value={c.status || 'UNKNOWN'} />
                    </td>
                    <td className={tdCls}>{c.sarId ?? '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <p className="mt-1 text-sm text-neutral-500">No CTR reports.</p>
        )}
      </div>
    </section>
  );
}
