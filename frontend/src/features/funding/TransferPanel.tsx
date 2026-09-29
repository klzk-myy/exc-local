/**
 * Internal transfer tab — Task 10.3.23 item 3 (Phase-05 Task 5.3.23).
 *   POST /transfers {from_account_id, to_account_id, currency, amount}
 *     — always with an Idempotency-Key (money-moving, §8.8)
 *   GET  /transfers — transfer history (bottom of panel)
 *   GET  /account/sub-accounts — the destination/source family members
 *   GET  /account/balances — live available for the selected currency
 */
import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import {
  ErrorBox,
  Field,
  StatusBadge,
  btnPrimary,
  cardCls,
  inputCls,
  selectCls,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import * as api from './api';

interface FamilyAccount {
  id: number;
  label: string;
}

function useFamilyAccounts(masterId: number | undefined) {
  const subs = useQuery({
    queryKey: ['account', 'sub-accounts'],
    queryFn: async () => {
      const res = await apiClient.get<{ data?: { id: number; status: string }[] }>(
        '/account/sub-accounts',
      );
      return res.data ?? [];
    },
  });
  const accounts: FamilyAccount[] = [];
  if (masterId !== undefined) accounts.push({ id: masterId, label: `Main account #${masterId}` });
  for (const s of subs.data ?? []) {
    accounts.push({ id: s.id, label: `Sub-account #${s.id}` });
  }
  return { accounts, subsError: subs.error, loading: subs.isPending };
}

export default function TransferPanel() {
  const qc = useQueryClient();
  const bals = useQuery({
    queryKey: ['account', 'balances'],
    queryFn: () => api.balances(apiClient),
  });
  const history = useQuery({
    queryKey: ['transfers'],
    queryFn: () => api.transferHistory(apiClient),
  });
  // account_id echoes back on the balances response — fetch it alongside.
  const masterIdQ = useQuery({
    queryKey: ['account', 'id'],
    queryFn: async () => {
      const res = await apiClient.get<{ account_id?: number }>('/account/balances');
      return res.account_id ?? 0;
    },
  });
  const { accounts } = useFamilyAccounts(masterIdQ.data);

  const [from, setFrom] = useState<number | ''>('');
  const [to, setTo] = useState<number | ''>('');
  const [currency, setCurrency] = useState('USD');
  const [amount, setAmount] = useState('');
  const [done, setDone] = useState<api.TransferResult | null>(null);

  const balance = (bals.data ?? []).find((b) => b.currency === currency);
  const exceeds =
    balance !== undefined &&
    amount !== '' &&
    Number.parseFloat(amount) > Number.parseFloat(balance.available);
  const sameAccount = from !== '' && to !== '' && from === to;

  const create = useMutation({
    mutationFn: () =>
      api.createTransfer(apiClient, {
        fromAccountId: Number(from),
        toAccountId: Number(to),
        currency,
        amount,
        idempotencyKey: api.newIdempotencyKey(),
      }),
    onSuccess: async (r) => {
      setDone(r);
      setAmount('');
      await qc.invalidateQueries({ queryKey: ['transfers'] });
      await qc.invalidateQueries({ queryKey: ['account', 'balances'] });
    },
  });

  const currencies = (bals.data ?? []).map((b) => b.currency);

  return (
    <div className="space-y-4">
      <div className={cardCls}>
        <h3 className="mb-1 text-sm font-semibold">Internal transfer</h3>
        <p className="mb-3 text-xs text-neutral-400">
          Instant, fee-free movement between your main account and sub-accounts — posted
          double-entry to the ledger.
        </p>
        {done !== null && (
          <p className="mb-3 text-sm text-emerald-400" role="status">
            Transfer {done.transfer.id} {done.transfer.status}
            {done.transfer.journal_entry_id !== undefined &&
              ` — journal entry ${done.transfer.journal_entry_id}`}
            .
          </p>
        )}
        <ErrorBox error={create.error} />
        <form
          onSubmit={(e) => {
            e.preventDefault();
            create.mutate();
          }}
        >
          <div className="grid grid-cols-2 gap-3">
            <Field label="From account" required>
              {(id, describedBy, invalid) => (
                <select
                  id={id}
                  aria-describedby={describedBy}
                  aria-invalid={invalid}
                  className={selectCls}
                  value={from}
                  onChange={(e) => {
                    setFrom(e.target.value === '' ? '' : Number(e.target.value));
                  }}
                >
                  <option value="">Select…</option>
                  {accounts.map((a) => (
                    <option key={a.id} value={a.id}>
                      {a.label}
                    </option>
                  ))}
                </select>
              )}
            </Field>
            <Field
              label="To account"
              required
              error={sameAccount ? 'Source and destination must differ' : null}
            >
              {(id, describedBy, invalid) => (
                <select
                  id={id}
                  aria-describedby={describedBy}
                  aria-invalid={invalid || sameAccount}
                  className={selectCls}
                  value={to}
                  onChange={(e) => {
                    setTo(e.target.value === '' ? '' : Number(e.target.value));
                  }}
                >
                  <option value="">Select…</option>
                  {accounts.map((a) => (
                    <option key={a.id} value={a.id}>
                      {a.label}
                    </option>
                  ))}
                </select>
              )}
            </Field>
          </div>
          <Field
            label="Currency"
            hint={balance !== undefined ? `Available: ${balance.available} ${currency}` : undefined}
          >
            {(id, describedBy, invalid) => (
              <select
                id={id}
                aria-describedby={describedBy}
                aria-invalid={invalid}
                className={selectCls}
                value={currency}
                onChange={(e) => {
                  setCurrency(e.target.value);
                }}
              >
                {(currencies.length > 0 ? currencies : ['USD']).map((c) => (
                  <option key={c} value={c}>
                    {c}
                  </option>
                ))}
              </select>
            )}
          </Field>
          <Field label="Amount" required error={exceeds ? 'Exceeds available balance' : null}>
            {(id, describedBy, invalid) => (
              <input
                id={id}
                aria-describedby={describedBy}
                aria-invalid={invalid || exceeds}
                className={inputCls}
                inputMode="decimal"
                placeholder="0.00"
                value={amount}
                onChange={(e) => {
                  setAmount(e.target.value);
                }}
              />
            )}
          </Field>
          <button
            type="submit"
            className={btnPrimary}
            disabled={
              create.isPending ||
              from === '' ||
              to === '' ||
              sameAccount ||
              amount === '' ||
              exceeds
            }
          >
            {create.isPending ? 'Transferring…' : 'Transfer'}
          </button>
        </form>
      </div>

      <div className={cardCls}>
        <h3 className="mb-2 text-sm font-semibold">Transfer history</h3>
        {history.isPending ? (
          <p className="text-sm text-neutral-400">Loading…</p>
        ) : (history.data?.data ?? []).length === 0 ? (
          <p className="text-sm text-neutral-400">No transfers yet.</p>
        ) : (
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>When</th>
                <th className={thCls}>From → To</th>
                <th className={thCls}>Amount</th>
                <th className={thCls}>Status</th>
              </tr>
            </thead>
            <tbody>
              {(history.data?.data ?? []).map((t) => (
                <tr key={t.id}>
                  <td className={tdCls}>{new Date(t.created_at).toLocaleString()}</td>
                  <td className={tdCls}>
                    {t.from_account_id} → {t.to_account_id}
                  </td>
                  <td className={tdCls}>
                    {t.amount} {t.currency}
                  </td>
                  <td className={tdCls}>
                    <StatusBadge value={t.status} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
