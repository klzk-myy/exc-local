/**
 * Employee-dealing panel (Phase-10.5 Task 10.5.3.5 §4) — the
 * pre-clearance request queue with independent-controller decisions,
 * the officer-filed request form, and the dealing audit trail
 * (employee_dealing.* / restricted_list.* admin-audit prefixes).
 * Approve/deny submits {id, approve, note} to the same
 * POST surface — the independent-control rule (requester ≠ decider) is
 * enforced server-side.
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
  labelCls,
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import {
  decidePreClearance,
  fetchDealingAudit,
  fetchPreClearances,
  filePreClearance,
  type PreClearance,
} from './api';

const OUTCOME_FILTERS = ['', 'PENDING', 'APPROVED', 'DENIED', 'EXPIRED'] as const;

export function EmployeeDealingPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [outcome, setOutcome] = useState<string>('PENDING');
  const [notice, setNotice] = useState<string | null>(null);
  const [deciding, setDeciding] = useState<PreClearance | null>(null);
  const [decisionNote, setDecisionNote] = useState('');

  const [file, setFile] = useState({
    accountId: '',
    instrument: '',
    side: '',
    reason: '',
    expiresAt: '',
  });

  const list = useQuery({
    queryKey: ['admin-pre-clearance', outcome, adminApi.env],
    queryFn: () => fetchPreClearances(adminApi, outcome),
  });
  const audit = useQuery({
    queryKey: ['admin-dealing-audit', adminApi.env],
    queryFn: () => fetchDealingAudit(adminApi),
  });

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-pre-clearance'] });
    void qc.invalidateQueries({ queryKey: ['admin-dealing-audit'] });
  };

  const decide = useMutation({
    mutationFn: (input: { id: number; approve: boolean }) =>
      decidePreClearance(adminApi, input.id, input.approve, decisionNote),
    onSuccess: (_v, input) => {
      setNotice(`Request ${input.id} ${input.approve ? 'approved' : 'denied'}.`);
      setDeciding(null);
      setDecisionNote('');
      invalidate();
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Decision failed'),
  });

  const fileReq = useMutation({
    mutationFn: () =>
      filePreClearance(adminApi, {
        accountId: Number(file.accountId),
        instrument: file.instrument,
        side: file.side === '' ? undefined : file.side,
        reason: file.reason,
        expiresAt: file.expiresAt === '' ? undefined : file.expiresAt,
      }),
    onSuccess: () => {
      setNotice('Pre-clearance request filed.');
      invalidate();
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'File failed'),
  });

  if (list.error !== null && isAccessDenied(list.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Employee dealing">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Employee dealing</h2>
        <div>
          <label className={labelCls} htmlFor="ed-outcome">
            Outcome
          </label>
          <select
            id="ed-outcome"
            className={selectCls}
            value={outcome}
            onChange={(e) => {
              setOutcome(e.target.value);
            }}
          >
            {OUTCOME_FILTERS.map((o) => (
              <option key={o} value={o}>
                {o === '' ? 'All' : o}
              </option>
            ))}
          </select>
        </div>
      </div>
      <p className={hintTextCls}>
        Pre-clearance decisions are independent-controlled — the requester cannot decide their own
        request (server-enforced).
      </p>

      {list.error !== null ? <ErrorBox error={list.error} /> : null}
      {list.data?.length === 0 ? (
        <p className="mt-2 text-sm text-neutral-500">No pre-clearance requests.</p>
      ) : null}
      {list.data !== undefined && list.data.length > 0 ? (
        <div className="mt-2 max-h-64 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Account</th>
                <th className={thCls}>Instrument</th>
                <th className={thCls}>Side</th>
                <th className={thCls}>Cap</th>
                <th className={thCls}>Outcome</th>
                <th className={thCls}>Expires</th>
                <th className={thCls}>
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {list.data.map((p) => (
                <tr key={p.id}>
                  <td className={tdCls} title={p.requestId}>
                    {p.id}
                  </td>
                  <td className={tdCls}>{p.accountId}</td>
                  <td className={tdCls}>{p.instrument === '' ? 'all' : p.instrument}</td>
                  <td className={tdCls}>{p.side ?? '—'}</td>
                  <td className={tdCls}>{p.notionalCap ?? '—'}</td>
                  <td className={tdCls}>
                    <StatusBadge value={p.outcome || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>{p.expiresAt}</td>
                  <td className={tdCls}>
                    {p.outcome === 'PENDING' ? (
                      <button
                        type="button"
                        className={btnGhost}
                        onClick={() => {
                          setDeciding(p);
                          setDecisionNote('');
                          setNotice(null);
                        }}
                      >
                        Decide…
                      </button>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      {deciding !== null ? (
        <div className="mt-3 space-y-2 rounded border border-neutral-700 p-3" aria-label="Decision">
          <p className="text-sm">
            Decide request <strong>#{deciding.id}</strong> — account {deciding.accountId} ·{' '}
            {deciding.reason}
          </p>
          <div>
            <label className={labelCls} htmlFor="ed-note">
              Decision note
            </label>
            <input
              id="ed-note"
              className={inputCls}
              value={decisionNote}
              onChange={(e) => {
                setDecisionNote(e.target.value);
              }}
            />
          </div>
          <div className="flex gap-2">
            <button
              type="button"
              className={btnPrimary}
              disabled={decide.isPending}
              onClick={() => decide.mutate({ id: deciding.id, approve: true })}
            >
              Approve
            </button>
            <button
              type="button"
              className={btnDanger}
              disabled={decide.isPending}
              onClick={() => decide.mutate({ id: deciding.id, approve: false })}
            >
              Deny
            </button>
            <button type="button" className={btnGhost} onClick={() => setDeciding(null)}>
              Cancel
            </button>
          </div>
        </div>
      ) : null}

      <form
        aria-label="File pre-clearance"
        className="mt-4 space-y-2 border-t border-neutral-800 pt-3"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(file.accountId) > 0 && file.reason !== '') fileReq.mutate();
        }}
      >
        <p className={hintTextCls}>Officer-filed request (on behalf of the desk)</p>
        <div className="grid grid-cols-1 gap-2 md:grid-cols-3">
          <div>
            <label className={labelCls} htmlFor="ed-acct">
              Account id
            </label>
            <input
              id="ed-acct"
              className={inputCls}
              value={file.accountId}
              onChange={(e) => {
                setFile({ ...file, accountId: e.target.value });
              }}
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="ed-inst">
              Instrument
            </label>
            <input
              id="ed-inst"
              className={inputCls}
              placeholder="blank = all covered"
              value={file.instrument}
              onChange={(e) => {
                setFile({ ...file, instrument: e.target.value });
              }}
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="ed-side">
              Side
            </label>
            <select
              id="ed-side"
              className={selectCls}
              value={file.side}
              onChange={(e) => {
                setFile({ ...file, side: e.target.value });
              }}
            >
              <option value="">Any</option>
              <option value="BUY">BUY</option>
              <option value="SELL">SELL</option>
            </select>
          </div>
          <div>
            <label className={labelCls} htmlFor="ed-exp">
              Expires at (RFC3339)
            </label>
            <input
              id="ed-exp"
              className={inputCls}
              value={file.expiresAt}
              onChange={(e) => {
                setFile({ ...file, expiresAt: e.target.value });
              }}
            />
          </div>
          <div className="md:col-span-2">
            <label className={labelCls} htmlFor="ed-reason">
              Reason
            </label>
            <input
              id="ed-reason"
              className={inputCls}
              value={file.reason}
              onChange={(e) => {
                setFile({ ...file, reason: e.target.value });
              }}
            />
          </div>
        </div>
        <button type="submit" className={btnPrimary} disabled={fileReq.isPending}>
          File request
        </button>
      </form>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}

      {audit.data !== undefined && audit.data.length > 0 ? (
        <div className="mt-4 border-t border-neutral-800 pt-3">
          <p className={hintTextCls}>Dealing audit trail (latest {audit.data.length})</p>
          <div className="mt-1 max-h-40 overflow-y-auto">
            <table className={tableCls}>
              <thead>
                <tr>
                  <th className={thCls}>ID</th>
                  <th className={thCls}>Officer</th>
                  <th className={thCls}>Action</th>
                  <th className={thCls}>Target</th>
                  <th className={thCls}>At</th>
                </tr>
              </thead>
              <tbody>
                {audit.data.map((a) => (
                  <tr key={a.id}>
                    <td className={tdCls}>{a.id}</td>
                    <td className={tdCls}>{a.adminUserId}</td>
                    <td className={tdCls}>{a.action}</td>
                    <td className={tdCls}>{a.targetType}</td>
                    <td className={tdCls}>{a.createdAt}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      ) : null}
    </section>
  );
}
