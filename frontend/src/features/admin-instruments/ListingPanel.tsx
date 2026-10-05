/**
 * Listing governance panel (Phase-10.5 Task 10.5.3.11 §1b) — listing
 * proposals: the queue (status-filtered) with the proposer's
 * auto-checks rendered verbatim, review actions (REVIEW / APPROVE /
 * REJECT — APPROVE files the OpInstrumentListing four-eyes request →
 * 202, REVIEW/REJECT apply immediately), and the proposal intake form.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

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
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import { createListingProposal, fetchListingProposals, reviewListingProposal } from './api';

export function ListingPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [statusFilter, setStatusFilter] = useState('PENDING');
  const [review, setReview] = useState({
    id: '',
    action: 'APPROVE' as 'REVIEW' | 'APPROVE' | 'REJECT',
    note: '',
  });
  const [form, setForm] = useState({
    symbol: '',
    oracleFeeds: '',
    reference:
      '{\n  "base_currency": "EUR",\n  "quote_currency": "USD",\n  "instrument_type": "SPOT",\n  "tick_size": "0.0001",\n  "lot_size": "100000",\n  "min_order_qty": "0.01",\n  "max_order_qty": "100",\n  "min_notional": "1000",\n  "contract_size": "100000",\n  "decimal_places": 5,\n  "pip_size": "0.0001"\n}',
    riskDefaults: '{\n  "max_leverage": 30\n}',
    reason: '',
  });
  const [notice, setNotice] = useState<string | null>(null);

  const list = useQuery({
    queryKey: ['admin-listing-proposals', statusFilter],
    queryFn: () => fetchListingProposals(adminApi, statusFilter === '' ? undefined : statusFilter),
  });
  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-listing-proposals'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const reviewMut = useMutation({
    mutationFn: () =>
      reviewListingProposal(adminApi, Number(review.id), {
        action: review.action,
        note: review.note,
      }),
    onSuccess: (r) => {
      setNotice(
        r === 'applied'
          ? `Proposal #${review.id} — ${review.action} applied.`
          : `APPROVE queued for four-eyes — request #${r.dualControlId} (${r.requiredApprover}); listing not created yet.`,
      );
      invalidate();
    },
    onError: onErr,
  });
  const create = useMutation({
    mutationFn: () =>
      createListingProposal(adminApi, {
        symbol: form.symbol,
        reference: JSON.parse(form.reference) as Record<string, unknown>,
        oracleFeeds: form.oracleFeeds
          .split(',')
          .map((s) => s.trim())
          .filter((s) => s !== ''),
        riskDefaults: JSON.parse(form.riskDefaults) as Record<string, unknown>,
        reason: form.reason,
      }),
    onSuccess: () => {
      setNotice('Proposal filed — auto-checks ran at insert.');
      invalidate();
    },
    onError: onErr,
  });

  if (list.error !== null && isAccessDenied(list.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Listing proposals">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Listing proposals</h2>
        <select
          aria-label="Proposal status filter"
          className={selectCls}
          value={statusFilter}
          onChange={(e) => {
            setStatusFilter(e.target.value);
          }}
        >
          <option value="">ALL</option>
          <option value="PENDING">PENDING</option>
          <option value="REVIEWED">REVIEWED</option>
          <option value="APPROVED">APPROVED</option>
          <option value="REJECTED">REJECTED</option>
          <option value="LISTED">LISTED</option>
        </select>
      </div>
      {list.error !== null ? <ErrorBox error={list.error} /> : null}
      {list.data?.length === 0 ? (
        <p className="text-sm text-neutral-500">No proposals for this status.</p>
      ) : null}
      {list.data !== undefined && list.data.length > 0 ? (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>ID</th>
              <th className={thCls}>Symbol</th>
              <th className={thCls}>Status</th>
              <th className={thCls}>Reason</th>
              <th className={thCls}>Auto-checks</th>
              <th className={thCls}>Activate at</th>
              <th className={thCls}>Instrument</th>
            </tr>
          </thead>
          <tbody>
            {list.data.map((p) => (
              <tr key={p.id}>
                <td className={tdCls}>{p.id}</td>
                <td className={tdCls}>{p.symbol}</td>
                <td className={tdCls}>
                  <StatusBadge value={p.status || 'UNKNOWN'} />
                </td>
                <td className={tdCls}>{p.reason}</td>
                <td className={tdCls}>
                  {p.autoChecks !== undefined && p.autoChecks.fails.length > 0
                    ? p.autoChecks.fails.join('; ')
                    : 'pass'}
                </td>
                <td className={tdCls}>{p.activateAt?.slice(0, 10) ?? 'next open'}</td>
                <td className={tdCls}>{p.instrumentId ?? '—'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}

      <form
        aria-label="Review proposal"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(review.id) > 0) reviewMut.mutate();
        }}
      >
        <input
          aria-label="Proposal id"
          className={inputCls}
          placeholder="proposal id"
          value={review.id}
          onChange={(e) => {
            setReview({ ...review, id: e.target.value });
          }}
        />
        <select
          aria-label="Review action"
          className={selectCls}
          value={review.action}
          onChange={(e) => {
            setReview({ ...review, action: e.target.value as typeof review.action });
          }}
        >
          <option value="REVIEW">REVIEW</option>
          <option value="APPROVE">APPROVE (4-eyes)</option>
          <option value="REJECT">REJECT</option>
        </select>
        <input
          aria-label="Review note"
          className={inputCls}
          placeholder="note"
          value={review.note}
          onChange={(e) => {
            setReview({ ...review, note: e.target.value });
          }}
        />
        <button
          type="submit"
          className={review.action === 'REJECT' ? btnDanger : btnPrimary}
          disabled={reviewMut.isPending}
        >
          Submit review
        </button>
      </form>

      <form
        aria-label="File listing proposal"
        className="mt-3 grid gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <div className="flex flex-wrap gap-2">
          <input
            aria-label="Proposal symbol"
            className={inputCls}
            placeholder="symbol (e.g. EURJPY)"
            value={form.symbol}
            onChange={(e) => {
              setForm({ ...form, symbol: e.target.value });
            }}
          />
          <input
            aria-label="Oracle feeds"
            className={inputCls}
            placeholder="oracle_feeds — ≥2, csv (refinitiv,bfix)"
            value={form.oracleFeeds}
            onChange={(e) => {
              setForm({ ...form, oracleFeeds: e.target.value });
            }}
          />
          <input
            aria-label="Proposal reason"
            className={inputCls}
            placeholder="reason"
            value={form.reason}
            onChange={(e) => {
              setForm({ ...form, reason: e.target.value });
            }}
          />
        </div>
        <div className="grid gap-2 md:grid-cols-2">
          <label className="text-xs">
            reference (JSON)
            <textarea
              aria-label="Reference JSON"
              className={`${inputCls} mt-1 h-36 font-mono`}
              value={form.reference}
              onChange={(e) => {
                setForm({ ...form, reference: e.target.value });
              }}
            />
          </label>
          <label className="text-xs">
            risk_defaults (JSON)
            <textarea
              aria-label="Risk defaults JSON"
              className={`${inputCls} mt-1 h-36 font-mono`}
              value={form.riskDefaults}
              onChange={(e) => {
                setForm({ ...form, riskDefaults: e.target.value });
              }}
            />
          </label>
        </div>
        <div>
          <button type="submit" className={btnGhost} disabled={create.isPending}>
            File proposal
          </button>
        </div>
      </form>
      <p className={hintTextCls}>
        Auto-checks (reference validity, symbol availability, ≥2 oracle feeds, risk defaults) run at
        insert and are stored on the row. APPROVE is dual-control; the instrument is created inside
        the approval transaction.
      </p>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
