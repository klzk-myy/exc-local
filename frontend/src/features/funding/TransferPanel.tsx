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
import { cmpDecimal, useValidatedField, type FieldRule } from '@/lib/input-helpers';
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

const RULE_ACCOUNT = (name: string, label: string): FieldRule => ({
  name,
  label,
  required: true,
  kind: 'string',
});
const RULE_AMOUNT: FieldRule = {
  name: 'amount',
  label: 'Amount',
  required: true,
  kind: 'decimal',
  positive: true,
};

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

  const from = useValidatedField(RULE_ACCOUNT('from_account_id', 'From account'));
  const to = useValidatedField(RULE_ACCOUNT('to_account_id', 'To account'));
  const [currency, setCurrency] = useState('USD');
  const amount = useValidatedField(RULE_AMOUNT);
  const [done, setDone] = useState<api.TransferResult | null>(null);

  const balance = (bals.data ?? []).find((b) => b.currency === currency);
  // Business rule, not structural: amount vs available balance stays
  // local (server authoritative); cmpDecimal is null-safe on garbage.
  const exceeds =
    balance !== undefined &&
    amount.value !== '' &&
    cmpDecimal(amount.value, balance.available) === 1;
  const sameAccount = from.value !== '' && to.value !== '' && from.value === to.value;

  const create = useMutation({
    mutationFn: () =>
      api.createTransfer(apiClient, {
        fromAccountId: Number(from.value),
        toAccountId: Number(to.value),
        currency,
        amount: amount.value,
        idempotencyKey: api.newIdempotencyKey(),
      }),
    onSuccess: async (r) => {
      setDone(r);
      amount.reset();
      await qc.invalidateQueries({ queryKey: ['transfers'] });
      await qc.invalidateQueries({ queryKey: ['account', 'balances'] });
    },
  });

  const currencies = (bals.data ?? []).map((b) => b.currency);

  return (
    <div className="space-y-4">
      <div className={cardCls}>
        <h2 className="mb-1 text-sm font-semibold">Internal transfer</h2>
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
            <Field label="From account" required error={from.error}>
              {(id, describedBy) => (
                <select
                  id={id}
                  aria-describedby={describedBy}
                  className={selectCls}
                  {...from.inputProps}
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
              error={to.error ?? (sameAccount ? 'Source and destination must differ' : null)}
            >
              {(id, describedBy) => (
                <select
                  id={id}
                  aria-describedby={describedBy}
                  className={selectCls}
                  {...to.inputProps}
                  aria-invalid={sameAccount || to.inputProps['aria-invalid']}
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
          <Field
            label="Amount"
            required
            error={amount.error ?? (exceeds ? 'Exceeds available balance' : null)}
          >
            {(id, describedBy) => (
              <input
                id={id}
                aria-describedby={describedBy}
                className={inputCls}
                inputMode="decimal"
                placeholder="0.00"
                {...amount.inputProps}
                aria-invalid={exceeds || amount.inputProps['aria-invalid']}
              />
            )}
          </Field>
          <button
            type="submit"
            className={btnPrimary}
            disabled={
              create.isPending ||
              !from.valid ||
              !to.valid ||
              sameAccount ||
              !amount.valid ||
              exceeds
            }
          >
            {create.isPending ? 'Transferring…' : 'Transfer'}
          </button>
        </form>
      </div>

      <div className={cardCls}>
        <h2 className="mb-2 text-sm font-semibold">Transfer history</h2>
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
