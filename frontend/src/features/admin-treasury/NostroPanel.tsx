/**
 * Nostro panel (Phase-10.5 Task 10.5.3.8 §1) — house-money registry and
 * coverage: per-currency nostro totals vs pending-withdrawal load
 * (deficit flag), the NOSTRO/VOSTRO account registry + create form,
 * the reserve→operating replenishment workflow (request → four-eyes
 * decide executes the two-sided movement), and the immutable SWIFT
 * message journal.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
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
  createNostroAccount,
  decideReplenishment,
  fetchNostroAccounts,
  fetchNostroCoverage,
  fetchReplenishments,
  fetchSwiftMessages,
  requestReplenishment,
} from './api';

export function NostroPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [acctForm, setAcctForm] = useState({
    currency: 'USD',
    bankName: '',
    bankCode: '',
    iban: '',
    role: 'NOSTRO',
  });
  const [repForm, setRepForm] = useState({
    currency: 'USD',
    amount: '',
    sourceId: '',
    targetId: '',
  });
  const [deciding, setDeciding] = useState<number | null>(null);
  const [decideNote, setDecideNote] = useState('');
  const [notice, setNotice] = useState<string | null>(null);

  const coverage = useQuery({
    queryKey: ['admin-nostro-coverage'],
    queryFn: () => fetchNostroCoverage(adminApi),
  });
  const accounts = useQuery({
    queryKey: ['admin-nostro-accounts'],
    queryFn: () => fetchNostroAccounts(adminApi, {}),
  });
  const reps = useQuery({
    queryKey: ['admin-nostro-replenishments'],
    queryFn: () => fetchReplenishments(adminApi),
  });
  const swift = useQuery({
    queryKey: ['admin-swift-journal'],
    queryFn: () => fetchSwiftMessages(adminApi, { limit: 30 }),
  });

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-nostro-accounts'] });
    void qc.invalidateQueries({ queryKey: ['admin-nostro-replenishments'] });
    void qc.invalidateQueries({ queryKey: ['admin-nostro-coverage'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const create = useMutation({
    mutationFn: () =>
      createNostroAccount(adminApi, {
        currency: acctForm.currency,
        bankName: acctForm.bankName,
        bankCode: acctForm.bankCode === '' ? undefined : acctForm.bankCode,
        iban: acctForm.iban === '' ? undefined : acctForm.iban,
        role: acctForm.role,
      }),
    onSuccess: () => {
      setNotice('Nostro account registered.');
      invalidate();
    },
    onError: onErr,
  });
  const request = useMutation({
    mutationFn: () =>
      requestReplenishment(adminApi, {
        currency: repForm.currency,
        amount: repForm.amount,
        sourceNostroId: Number(repForm.sourceId),
        targetNostroId: Number(repForm.targetId),
      }),
    onSuccess: (r) => {
      setNotice(
        `Replenishment #${r.id} opened — ${r.status}. A distinct approver must decide before funds move.`,
      );
      invalidate();
    },
    onError: onErr,
  });
  const decide = useMutation({
    mutationFn: (input: { id: number; action: 'APPROVE' | 'REJECT' }) =>
      decideReplenishment(adminApi, input.id, {
        action: input.action,
        note: decideNote === '' ? undefined : decideNote,
      }),
    onSuccess: (r) => {
      setNotice(`Replenishment #${r.id} → ${r.status}.`);
      setDeciding(null);
      invalidate();
    },
    onError: onErr,
  });

  const denied =
    (coverage.error !== null && isAccessDenied(coverage.error)) ||
    (accounts.error !== null && isAccessDenied(accounts.error));
  if (denied) return <AccessDeniedCard />;

  return (
    <section className={cardCls} aria-label="Nostro operations">
      <h2 className="mb-1 text-sm font-semibold">Nostro coverage &amp; registry</h2>

      {coverage.error !== null ? <ErrorBox error={coverage.error} /> : null}
      {coverage.data !== undefined && coverage.data.length > 0 ? (
        <table className={tableCls} aria-label="Nostro coverage">
          <thead>
            <tr>
              <th className={thCls}>Currency</th>
              <th className={thCls}>Nostro total</th>
              <th className={thCls}>Confirmed due</th>
              <th className={thCls}>Queued</th>
              <th className={thCls}>Coverage</th>
            </tr>
          </thead>
          <tbody>
            {coverage.data.map((c) => (
              <tr key={c.currency}>
                <td className={tdCls}>{c.currency}</td>
                <td className={tdCls}>{c.nostroTotal}</td>
                <td className={tdCls}>{c.confirmedDue}</td>
                <td className={tdCls}>{c.queued}</td>
                <td className={tdCls}>
                  <StatusBadge value={c.deficit ? 'DEFICIT' : 'COVERED'} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}

      <h3 className="mb-1 mt-4 text-xs font-semibold uppercase tracking-wide text-neutral-400">
        Account registry
      </h3>
      {accounts.data?.length === 0 ? (
        <p className="text-sm text-neutral-500">No nostro accounts registered.</p>
      ) : null}
      {accounts.data !== undefined && accounts.data.length > 0 ? (
        <div className="max-h-44 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Bank</th>
                <th className={thCls}>CCY</th>
                <th className={thCls}>IBAN</th>
                <th className={thCls}>Role</th>
                <th className={thCls}>Balance</th>
                <th className={thCls}>Status</th>
              </tr>
            </thead>
            <tbody>
              {accounts.data.map((a) => (
                <tr key={a.id}>
                  <td className={tdCls}>{a.id}</td>
                  <td className={tdCls}>{a.bankName}</td>
                  <td className={tdCls}>{a.currency}</td>
                  <td className={tdCls}>{a.iban ?? '—'}</td>
                  <td className={tdCls}>{a.role}</td>
                  <td className={tdCls}>{a.balance}</td>
                  <td className={tdCls}>
                    <StatusBadge value={a.status || 'UNKNOWN'} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      <form
        aria-label="Register nostro account"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (acctForm.bankName !== '') create.mutate();
        }}
      >
        <div>
          <label className={labelCls} htmlFor="na-bank">
            Bank
          </label>
          <input
            id="na-bank"
            className={inputCls}
            value={acctForm.bankName}
            onChange={(e) => {
              setAcctForm({ ...acctForm, bankName: e.target.value });
            }}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="na-ccy">
            Currency
          </label>
          <input
            id="na-ccy"
            className={inputCls}
            value={acctForm.currency}
            onChange={(e) => {
              setAcctForm({ ...acctForm, currency: e.target.value });
            }}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="na-bic">
            BIC
          </label>
          <input
            id="na-bic"
            className={inputCls}
            value={acctForm.bankCode}
            onChange={(e) => {
              setAcctForm({ ...acctForm, bankCode: e.target.value });
            }}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="na-iban">
            IBAN
          </label>
          <input
            id="na-iban"
            className={inputCls}
            value={acctForm.iban}
            onChange={(e) => {
              setAcctForm({ ...acctForm, iban: e.target.value });
            }}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="na-role">
            Role
          </label>
          <select
            id="na-role"
            className={selectCls}
            value={acctForm.role}
            onChange={(e) => {
              setAcctForm({ ...acctForm, role: e.target.value });
            }}
          >
            <option value="NOSTRO">NOSTRO</option>
            <option value="VOSTRO">VOSTRO</option>
          </select>
        </div>
        <button type="submit" className={btnGhost} disabled={create.isPending}>
          Register
        </button>
      </form>

      <h3 className="mb-1 mt-4 text-xs font-semibold uppercase tracking-wide text-neutral-400">
        Replenishments (reserve → operating)
      </h3>
      {reps.data?.length === 0 ? (
        <p className="text-sm text-neutral-500">No replenishment requests.</p>
      ) : null}
      {reps.data !== undefined && reps.data.length > 0 ? (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>ID</th>
              <th className={thCls}>CCY</th>
              <th className={thCls}>Amount</th>
              <th className={thCls}>Source→Target</th>
              <th className={thCls}>Status</th>
              <th className={thCls}>
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {reps.data.map((r) => (
              <tr key={r.id}>
                <td className={tdCls}>{r.id}</td>
                <td className={tdCls}>{r.currency}</td>
                <td className={tdCls}>{r.amount}</td>
                <td className={tdCls}>
                  {r.sourceNostroId}→{r.targetNostroId}
                </td>
                <td className={tdCls}>
                  <StatusBadge value={r.status || 'UNKNOWN'} />
                </td>
                <td className={tdCls}>
                  {r.status === 'PENDING_APPROVAL' ? (
                    <button
                      type="button"
                      className={btnGhost}
                      onClick={() => {
                        setDeciding(deciding === r.id ? null : r.id);
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
      ) : null}
      {deciding !== null ? (
        <div className="mt-2 flex flex-wrap items-center gap-2 rounded border border-neutral-700 p-2">
          <span className="text-sm">Decide #{deciding} (four-eyes):</span>
          <input
            aria-label="Decision note"
            className={inputCls}
            placeholder="note"
            value={decideNote}
            onChange={(e) => {
              setDecideNote(e.target.value);
            }}
          />
          <button
            type="button"
            className={btnPrimary}
            disabled={decide.isPending}
            onClick={() => {
              decide.mutate({ id: deciding, action: 'APPROVE' });
            }}
          >
            Approve &amp; execute
          </button>
          <button
            type="button"
            className={btnGhost}
            disabled={decide.isPending}
            onClick={() => {
              decide.mutate({ id: deciding, action: 'REJECT' });
            }}
          >
            Reject
          </button>
        </div>
      ) : null}
      <form
        aria-label="Request replenishment"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (
            repForm.amount !== '' &&
            Number(repForm.sourceId) > 0 &&
            Number(repForm.targetId) > 0
          ) {
            request.mutate();
          }
        }}
      >
        <div>
          <label className={labelCls} htmlFor="rp-ccy">
            Currency
          </label>
          <input
            id="rp-ccy"
            className={inputCls}
            value={repForm.currency}
            onChange={(e) => {
              setRepForm({ ...repForm, currency: e.target.value });
            }}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="rp-amt">
            Amount
          </label>
          <input
            id="rp-amt"
            className={inputCls}
            value={repForm.amount}
            onChange={(e) => {
              setRepForm({ ...repForm, amount: e.target.value });
            }}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="rp-src">
            Source nostro id
          </label>
          <input
            id="rp-src"
            className={inputCls}
            value={repForm.sourceId}
            onChange={(e) => {
              setRepForm({ ...repForm, sourceId: e.target.value });
            }}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="rp-tgt">
            Target nostro id
          </label>
          <input
            id="rp-tgt"
            className={inputCls}
            value={repForm.targetId}
            onChange={(e) => {
              setRepForm({ ...repForm, targetId: e.target.value });
            }}
          />
        </div>
        <button type="submit" className={btnPrimary} disabled={request.isPending}>
          Request (opens PENDING_APPROVAL)
        </button>
      </form>
      <p className={hintTextCls}>
        Requests open PENDING_APPROVAL; a distinct approver's APPROVE executes the two-sided
        movement atomically.
      </p>

      <h3 className="mb-1 mt-4 text-xs font-semibold uppercase tracking-wide text-neutral-400">
        SWIFT journal (immutable)
      </h3>
      {swift.data !== undefined && swift.data.length > 0 ? (
        <div className="max-h-40 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Type</th>
                <th className={thCls}>:20: Ref</th>
                <th className={thCls}>Dir</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>At</th>
              </tr>
            </thead>
            <tbody>
              {swift.data.map((m) => (
                <tr key={m.id}>
                  <td className={tdCls}>{m.messageType}</td>
                  <td className={tdCls}>{m.reference}</td>
                  <td className={tdCls}>{m.direction}</td>
                  <td className={tdCls}>
                    <StatusBadge value={m.status || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>{m.msgTimestamp}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="text-sm text-neutral-500">No SWIFT messages in window.</p>
      )}
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
