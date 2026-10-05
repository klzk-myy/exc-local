/**
 * Bank accounts tab — beneficiary registry (Task 11.3.7), withdrawal
 * whitelist mode (11.3.10), rail capability matrix + selection preview
 * (11.3.1).
 *
 *   GET/POST/DELETE /funding/bank-accounts     register/list/remove (?id=)
 *   GET/POST         /funding/withdrawal-whitelist[/enable|/disable]
 *   GET              /funding/rails            capability matrix
 *   POST             /funding/rail-selection   chosen rail + value date
 *
 * Whitelist disable latches a 24h account-scoped egress lock — the
 * countdown renders from `withdrawal_lock_until`; premature re-enable is
 * refused server-side (WHITELIST_CHANGE_LOCKED) and surfaced verbatim.
 */
import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import {
  ErrorBox,
  Field,
  StatusBadge,
  btnGhost,
  btnPrimary,
  cardCls,
  formatCountdown,
  inputCls,
  selectCls,
  tableCls,
  tdCls,
  thCls,
  useNow,
} from '@/lib/ui';

import * as api from './api';

const FIAT = ['USD', 'EUR', 'GBP', 'CHF', 'JPY', 'CAD', 'AUD', 'SEK', 'NOK'] as const;
const RAILS = ['SWIFT', 'SEPA', 'FEDNOW', 'ACH', 'CHAPS', 'TARGET2'] as const;

function LockCountdown({ until }: { until: string }) {
  const now = useNow(1000);
  const left = Date.parse(until) - now;
  if (left <= 0) return null;
  return (
    <span className="font-mono text-xs text-amber-300" role="timer">
      {formatCountdown(left)} remaining
    </span>
  );
}

function WhitelistCard() {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ['funding', 'whitelist'],
    queryFn: () => api.withdrawalWhitelist(apiClient),
  });
  const setMode = useMutation({
    mutationFn: (enable: boolean) => api.setWhitelistMode(apiClient, enable),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ['funding', 'whitelist'] });
    },
  });

  const v = q.data;
  const whitelistOn = v?.whitelist_only_enabled === true || v?.mode === 'WHITELIST_ONLY';
  return (
    <div className={cardCls}>
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-sm font-semibold">Withdrawal whitelist</h2>
        {v !== undefined && <StatusBadge value={v.mode} />}
      </div>
      <ErrorBox error={q.error} />
      <ErrorBox error={setMode.error} />
      {v !== undefined && (
        <>
          <p className="text-xs text-neutral-400">
            {whitelistOn
              ? 'Withdrawals may only go to VERIFIED beneficiaries below.'
              : 'Any beneficiary account may receive withdrawals.'}
          </p>
          {v.withdrawals_locked === true && v.withdrawal_lock_until !== undefined && (
            <p className="mt-2 rounded border border-amber-700/50 bg-amber-950/40 p-2 text-xs text-amber-300">
              Withdrawals locked after whitelist disable —{' '}
              <LockCountdown until={v.withdrawal_lock_until} />
            </p>
          )}
          {v.reenable_locked === true && (
            <p className="mt-2 text-xs text-amber-300">
              Re-enabling is rate-limited while the change lock stands.
            </p>
          )}
          <button
            type="button"
            className={`${btnGhost} mt-3`}
            disabled={setMode.isPending}
            onClick={() => {
              setMode.mutate(!whitelistOn);
            }}
          >
            {whitelistOn ? 'Disable whitelist' : 'Enable whitelist-only mode'}
          </button>
          {(v.beneficiaries ?? []).length > 0 && (
            <ul className="mt-3 space-y-1 text-xs text-neutral-300">
              {(v.beneficiaries ?? []).map((b) => (
                <li key={b.bank_account_id} className="flex justify-between">
                  <span>
                    {b.beneficiary_name} · {b.iban ?? b.account_number ?? '—'} · {b.currency}
                  </span>
                  <StatusBadge value={b.status} />
                </li>
              ))}
            </ul>
          )}
        </>
      )}
    </div>
  );
}

function RegisterForm() {
  const qc = useQueryClient();
  const [currency, setCurrency] = useState('USD');
  const [rail, setRail] = useState<string>('SWIFT');
  const [bankName, setBankName] = useState('');
  const [beneficiary, setBeneficiary] = useState('');
  const [iban, setIban] = useState('');
  const [accountNumber, setAccountNumber] = useState('');
  const [swiftBic, setSwiftBic] = useState('');
  const [bicRouting, setBicRouting] = useState('');

  const register = useMutation({
    mutationFn: () =>
      api.registerBankAccount(apiClient, {
        currency,
        rail,
        bank_name: bankName,
        beneficiary_name: beneficiary,
        iban: iban === '' ? undefined : iban,
        account_number: accountNumber === '' ? undefined : accountNumber,
        swift_bic: swiftBic === '' ? undefined : swiftBic,
        bic_routing: bicRouting === '' ? undefined : bicRouting,
      }),
    onSuccess: async () => {
      setBankName('');
      setBeneficiary('');
      setIban('');
      setAccountNumber('');
      setSwiftBic('');
      setBicRouting('');
      await qc.invalidateQueries({ queryKey: ['funding', 'bank-accounts'] });
    },
  });

  const valid = bankName !== '' && beneficiary !== '' && (iban !== '' || accountNumber !== '');
  return (
    <form
      className="mt-3 grid gap-3 sm:grid-cols-2"
      onSubmit={(e) => {
        e.preventDefault();
        register.mutate();
      }}
    >
      <Field label="Currency">
        {(id) => (
          <select
            id={id}
            className={selectCls}
            value={currency}
            onChange={(e) => {
              setCurrency(e.target.value);
            }}
          >
            {FIAT.map((c) => (
              <option key={c} value={c}>
                {c}
              </option>
            ))}
          </select>
        )}
      </Field>
      <Field label="Rail">
        {(id) => (
          <select
            id={id}
            className={selectCls}
            value={rail}
            onChange={(e) => {
              setRail(e.target.value);
            }}
          >
            {RAILS.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
        )}
      </Field>
      <Field label="Bank name" required>
        {(id) => (
          <input
            id={id}
            className={inputCls}
            value={bankName}
            onChange={(e) => {
              setBankName(e.target.value);
            }}
          />
        )}
      </Field>
      <Field label="Beneficiary name" required>
        {(id) => (
          <input
            id={id}
            className={inputCls}
            value={beneficiary}
            onChange={(e) => {
              setBeneficiary(e.target.value);
            }}
          />
        )}
      </Field>
      <Field label="IBAN" hint="IBAN or account number required">
        {(id) => (
          <input
            id={id}
            className={inputCls}
            value={iban}
            onChange={(e) => {
              setIban(e.target.value);
            }}
          />
        )}
      </Field>
      <Field label="Account number">
        {(id) => (
          <input
            id={id}
            className={inputCls}
            value={accountNumber}
            onChange={(e) => {
              setAccountNumber(e.target.value);
            }}
          />
        )}
      </Field>
      <Field label="SWIFT/BIC">
        {(id) => (
          <input
            id={id}
            className={inputCls}
            value={swiftBic}
            onChange={(e) => {
              setSwiftBic(e.target.value);
            }}
          />
        )}
      </Field>
      <Field label="BIC routing">
        {(id) => (
          <input
            id={id}
            className={inputCls}
            value={bicRouting}
            onChange={(e) => {
              setBicRouting(e.target.value);
            }}
          />
        )}
      </Field>
      <div className="sm:col-span-2">
        <ErrorBox error={register.error} />
        <button type="submit" className={btnPrimary} disabled={register.isPending || !valid}>
          {register.isPending ? 'Registering…' : 'Register bank account'}
        </button>
        <p className="mt-1 text-xs text-neutral-500">
          New accounts start PENDING_VERIFICATION — withdrawals unlock only after admin verify.
        </p>
      </div>
    </form>
  );
}

function RailMatrix() {
  const rails = useQuery({
    queryKey: ['funding', 'rails'],
    queryFn: () => api.fundingRails(apiClient),
  });
  return (
    <div className={cardCls}>
      <h2 className="mb-2 text-sm font-semibold">Payment rails</h2>
      <ErrorBox error={rails.error} />
      {rails.isPending ? (
        <p className="text-sm text-neutral-400">Loading…</p>
      ) : (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>Rail</th>
              <th className={thCls}>Currencies</th>
              <th className={thCls}>Cut-off</th>
              <th className={thCls}>Settles</th>
              <th className={thCls}>Instant</th>
            </tr>
          </thead>
          <tbody>
            {(rails.data ?? []).map((r) => (
              <tr key={r.rail}>
                <td className={tdCls}>
                  <span className="font-medium">{r.rail}</span>
                  <span className="ml-1 text-neutral-500">{r.name}</span>
                </td>
                <td className={tdCls}>
                  {r.all_currencies === true ? 'all' : (r.currencies ?? []).join(', ')}
                </td>
                <td className={tdCls}>{r.cutoff_label ?? '—'}</td>
                <td className={tdCls}>{r.settlement_lag}</td>
                <td className={tdCls}>
                  {r.instant_capable === true
                    ? `yes${r.max_amount !== undefined ? ` ≤ ${r.max_amount}` : ''}`
                    : '—'}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

function RailPicker() {
  const [currency, setCurrency] = useState('USD');
  const [amount, setAmount] = useState('');
  const [preferred, setPreferred] = useState('');
  const [sameDay, setSameDay] = useState(false);
  const pick = useMutation({
    mutationFn: () =>
      api.railSelection(apiClient, {
        currency,
        amount,
        preferredRail: preferred === '' ? undefined : preferred,
        requireSameDay: sameDay,
      }),
  });

  return (
    <div className={cardCls}>
      <h2 className="mb-2 text-sm font-semibold">Rail selection preview</h2>
      <form
        className="grid gap-3 sm:grid-cols-4"
        onSubmit={(e) => {
          e.preventDefault();
          pick.mutate();
        }}
      >
        <Field label="Payment currency">
          {(id) => (
            <select
              id={id}
              className={selectCls}
              value={currency}
              onChange={(e) => {
                setCurrency(e.target.value);
              }}
            >
              {FIAT.map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </select>
          )}
        </Field>
        <Field label="Preview amount" required>
          {(id) => (
            <input
              id={id}
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
        <Field label="Preferred rail">
          {(id) => (
            <select
              id={id}
              className={selectCls}
              value={preferred}
              onChange={(e) => {
                setPreferred(e.target.value);
              }}
            >
              <option value="">auto</option>
              {RAILS.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </select>
          )}
        </Field>
        <Field label="Same-day value">
          {(id) => (
            <input
              id={id}
              type="checkbox"
              className="h-6 w-6"
              checked={sameDay}
              onChange={(e) => {
                setSameDay(e.target.checked);
              }}
            />
          )}
        </Field>
        <div className="sm:col-span-4">
          <button type="submit" className={btnPrimary} disabled={pick.isPending || amount === ''}>
            {pick.isPending ? 'Selecting…' : 'Preview rail'}
          </button>
        </div>
      </form>
      <ErrorBox error={pick.error} />
      {pick.isSuccess && (
        <div className="mt-3 rounded border border-neutral-800 p-3 text-sm" role="status">
          <p>
            Selected <strong>{pick.data.rail}</strong>
            {pick.data.value_date !== undefined &&
              ` — value date ${new Date(pick.data.value_date).toLocaleDateString()}`}
            {pick.data.queued_next_day === true && ' (queued for next business day)'}
          </p>
          {(pick.data.rejected_rails ?? []).length > 0 && (
            <p className="mt-1 text-xs text-neutral-400">
              Rejected: {(pick.data.rejected_rails ?? []).join(', ')}
            </p>
          )}
        </div>
      )}
    </div>
  );
}

export default function AccountsPanel() {
  const qc = useQueryClient();
  const accounts = useQuery({
    queryKey: ['funding', 'bank-accounts'],
    queryFn: () => api.bankAccounts(apiClient),
  });
  const remove = useMutation({
    mutationFn: (id: number) => api.deleteBankAccount(apiClient, id),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ['funding', 'bank-accounts'] });
      await qc.invalidateQueries({ queryKey: ['funding', 'whitelist'] });
    },
  });

  return (
    <div className="space-y-4">
      <div className={cardCls}>
        <h2 className="mb-2 text-sm font-semibold">Beneficiary bank accounts</h2>
        <ErrorBox error={accounts.error} />
        {accounts.isPending ? (
          <p className="text-sm text-neutral-400">Loading…</p>
        ) : (accounts.data ?? []).length === 0 ? (
          <p className="text-sm text-neutral-400">No bank accounts registered.</p>
        ) : (
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Beneficiary</th>
                <th className={thCls}>Account</th>
                <th className={thCls}>Rail</th>
                <th className={thCls}>Status</th>
                <th className={thCls} />
              </tr>
            </thead>
            <tbody>
              {(accounts.data ?? []).map((b) => (
                <tr key={b.bank_account_id}>
                  <td className={tdCls}>
                    <span className="font-medium">{b.beneficiary_name}</span>
                    <span className="ml-1 text-neutral-500">
                      {b.bank_name} · {b.currency}
                    </span>
                  </td>
                  <td className={`${tdCls} font-mono text-xs`}>
                    {b.iban ?? b.account_number ?? '—'}
                    {b.swift_bic !== undefined && ` · ${b.swift_bic}`}
                  </td>
                  <td className={tdCls}>{b.rail}</td>
                  <td className={tdCls}>
                    <StatusBadge value={b.status} />
                    {b.status === 'REJECTED' && b.rejection_reason !== undefined && (
                      <span className="ml-1 text-xs text-red-400">{b.rejection_reason}</span>
                    )}
                  </td>
                  <td className={tdCls}>
                    <button
                      type="button"
                      className={btnGhost}
                      disabled={remove.isPending}
                      onClick={() => {
                        remove.mutate(b.bank_account_id);
                      }}
                    >
                      Remove
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        <ErrorBox error={remove.error} />
        <RegisterForm />
      </div>
      <WhitelistCard />
      <RailMatrix />
      <RailPicker />
    </div>
  );
}
