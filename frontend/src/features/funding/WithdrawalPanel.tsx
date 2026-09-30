/**
 * Withdrawal tab — Task 10.3.23 item 2.
 *   POST /withdrawals           → creates PENDING + mints a confirm token
 *                                 (exactly-15-minute window from create)
 *   POST /withdrawals/{id}/confirm → CONFIRMED or PENDING_REVIEW per tier
 *
 * Canonical tiers surfaced verbatim: <$10K AUTO · $10K–$50K STANDARD ·
 * >$50K PENDING_REVIEW with a 4-hour review deadline. An unconfirmed
 * withdrawal auto-cancels at window expiry (backend releases the hold).
 */
import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { cmpDecimal, useValidatedField, type FieldRule } from '@/lib/input-helpers';
import {
  CopyButton,
  ErrorBox,
  Field,
  StatusBadge,
  btnGhost,
  btnPrimary,
  cardCls,
  formatCountdown,
  inputCls,
  selectCls,
  useNow,
} from '@/lib/ui';

import * as api from './api';

const RULE_AMOUNT: FieldRule = {
  name: 'amount',
  label: 'Amount',
  required: true,
  kind: 'decimal',
  positive: true,
};
const RULE_REFERENCE: FieldRule = {
  name: 'reference_account',
  label: 'Beneficiary account',
  required: true,
  kind: 'string',
};
const RULE_TOKEN: FieldRule = {
  name: 'token',
  label: 'Confirmation token',
  required: true,
  kind: 'string',
};

function TierNote({ tier }: { tier?: string }) {
  const t = api.REVIEW_TIERS.find((x) => x.tier === tier);
  if (t === undefined) return null;
  return (
    <p className="mt-1 text-xs text-neutral-400" role="note">
      {t.range}: {t.note}
    </p>
  );
}

/** Countdown to the 15-minute confirmation deadline. */
function ConfirmCountdown({ createdOrExpiry }: { createdOrExpiry: string }) {
  const now = useNow(1000);
  // expires_at is authoritative when present; else created_at + 15min.
  const expiry = Date.parse(createdOrExpiry);
  const left = expiry - now;
  if (Number.isNaN(expiry)) return <span className="text-xs text-neutral-500">—</span>;
  if (left <= 0)
    return (
      <span className="text-xs text-red-400" role="timer">
        expired — auto-cancelled
      </span>
    );
  return (
    <span className="font-mono text-xs text-amber-300" role="timer">
      {formatCountdown(left)} remaining
    </span>
  );
}

function PendingWithdrawalRow({ row }: { row: api.FundingTxRow }) {
  const qc = useQueryClient();
  const token = useValidatedField(RULE_TOKEN);
  const confirm = useMutation({
    mutationFn: () => api.confirmWithdrawal(apiClient, row.id, token.value),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ['funding'] });
    },
  });
  // created_at + exactly 15 min (CONFIRM_WINDOW_MS); expires_at is
  // authoritative when the row carries it.
  const expiryMs = Date.parse(row.created_at) + api.CONFIRM_WINDOW_MS;
  const expiryIso = Number.isNaN(expiryMs) ? row.created_at : new Date(expiryMs).toISOString();
  return (
    <li className="rounded border border-neutral-800 p-3">
      <div className="flex items-center justify-between">
        <span className="text-sm">
          {row.amount} {row.currency} → {row.reference_account ?? 'beneficiary'}
        </span>
        <StatusBadge value={row.status} />
      </div>
      <div className="mt-1 flex items-center justify-between text-xs text-neutral-400">
        <span>#{row.id}</span>
        <ConfirmCountdown createdOrExpiry={expiryIso} />
      </div>
      {confirm.isSuccess && (
        <p className="mt-2 text-sm text-emerald-400" role="status">
          Confirmed — now {confirm.data.status}.
        </p>
      )}
      <ErrorBox error={confirm.error} />
      <form
        className="mt-2 flex gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          confirm.mutate();
        }}
      >
        <label className="sr-only" htmlFor={`tok-${row.id}`}>
          Confirmation token for withdrawal {row.id}
        </label>
        <input
          id={`tok-${row.id}`}
          className={`${inputCls} flex-1`}
          placeholder="Confirmation token (emailed)"
          {...token.inputProps}
        />
        <button type="submit" className={btnPrimary} disabled={confirm.isPending || !token.valid}>
          Confirm
        </button>
      </form>
    </li>
  );
}

export default function WithdrawalPanel() {
  const qc = useQueryClient();
  const bals = useQuery({
    queryKey: ['account', 'balances'],
    queryFn: () => api.balances(apiClient),
  });
  const pending = useQuery({
    queryKey: ['funding', 'pending-withdrawals'],
    queryFn: () =>
      api.fundingHistory(apiClient, { type: 'WITHDRAWAL', status: 'PENDING', limit: 20 }),
    refetchInterval: 30_000,
  });

  const [currency, setCurrency] = useState('USD');
  const amount = useValidatedField(RULE_AMOUNT);
  const reference = useValidatedField(RULE_REFERENCE);
  const [method, setMethod] = useState<string>('SWIFT');
  const [confirmVia, setConfirmVia] = useState<string>('email');
  const [created, setCreated] = useState<api.WithdrawalResult | null>(null);

  const balance = (bals.data ?? []).find((b) => b.currency === currency);
  // Business rule (server remains authoritative): amount vs available
  // balance — cmpDecimal returns null on non-decimal input → no flag.
  const exceeds =
    balance !== undefined &&
    amount.value !== '' &&
    cmpDecimal(amount.value, balance.available) === 1;

  const create = useMutation({
    mutationFn: () =>
      api.createWithdrawal(apiClient, {
        currency,
        amount: amount.value,
        referenceAccount: reference.value,
        bankMethod: method,
        confirmMethod: confirmVia,
        idempotencyKey: api.newIdempotencyKey(),
      }),
    onSuccess: async (r) => {
      setCreated(r);
      await qc.invalidateQueries({ queryKey: ['funding'] });
    },
  });

  return (
    <div className="space-y-4">
      <div className={cardCls}>
        <h3 className="mb-1 text-sm font-semibold">New withdrawal</h3>
        <p className="mb-3 text-xs text-neutral-400">
          Withdrawals require confirmation within <strong>15 minutes</strong> or they auto-cancel
          and release the hold. Review tiers: under $10K automatic · $10K–$50K standard screening ·
          over $50K manual review (4-hour deadline).
        </p>

        {created !== null && (
          <div
            className="mb-4 rounded border border-amber-700/50 bg-amber-950/40 p-3"
            role="status"
          >
            <p className="text-sm font-medium text-amber-300">
              Withdrawal #{created.withdrawal_id} created — status {created.status}.
            </p>
            {created.confirm_token !== undefined && created.confirm_token !== '' && (
              <p className="mt-1 flex items-center gap-2 font-mono text-xs text-amber-100">
                Confirm token (shown once): {created.confirm_token}
                <CopyButton text={created.confirm_token} label="Copy token" />
              </p>
            )}
            {created.expires_at !== undefined && (
              <ConfirmCountdown createdOrExpiry={created.expires_at} />
            )}
            <TierNote tier={created.review_tier} />
          </div>
        )}
        <ErrorBox error={create.error} />

        <form
          onSubmit={(e) => {
            e.preventDefault();
            create.mutate();
          }}
        >
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
                {(bals.data ?? [{ currency: 'USD' } as api.BalanceRow]).map((b) => (
                  <option key={b.currency} value={b.currency}>
                    {b.currency}
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
          <Field
            label="Beneficiary account (IBAN / account reference)"
            required
            error={reference.error}
            hint="Use a registered beneficiary account — third-party withdrawals are rejected."
          >
            {(id, describedBy) => (
              <input
                id={id}
                aria-describedby={describedBy}
                className={inputCls}
                {...reference.inputProps}
              />
            )}
          </Field>
          <div className="grid grid-cols-2 gap-3">
            <Field label="Rail">
              {(id, describedBy, invalid) => (
                <select
                  id={id}
                  aria-describedby={describedBy}
                  aria-invalid={invalid}
                  className={selectCls}
                  value={method}
                  onChange={(e) => {
                    setMethod(e.target.value);
                  }}
                >
                  {api.BANK_METHODS.filter((m) => m !== 'INTERNAL').map((m) => (
                    <option key={m} value={m}>
                      {m}
                    </option>
                  ))}
                </select>
              )}
            </Field>
            <Field label="Confirm via">
              {(id, describedBy, invalid) => (
                <select
                  id={id}
                  aria-describedby={describedBy}
                  aria-invalid={invalid}
                  className={selectCls}
                  value={confirmVia}
                  onChange={(e) => {
                    setConfirmVia(e.target.value);
                  }}
                >
                  {api.CONFIRM_METHODS.map((m) => (
                    <option key={m} value={m}>
                      {m === '2fa_totp' ? 'Authenticator (TOTP)' : m}
                    </option>
                  ))}
                </select>
              )}
            </Field>
          </div>
          <button
            type="submit"
            className={btnPrimary}
            disabled={create.isPending || !amount.valid || !reference.valid || exceeds}
          >
            {create.isPending ? 'Creating…' : 'Create withdrawal'}
          </button>
        </form>
      </div>

      <div className={cardCls}>
        <h3 className="mb-2 text-sm font-semibold">Awaiting your confirmation</h3>
        {(pending.data?.data ?? []).filter((r) => r.status === 'PENDING').length === 0 ? (
          <p className="text-sm text-neutral-400">No withdrawals awaiting confirmation.</p>
        ) : (
          <ul className="space-y-3">
            {pending.data?.data
              .filter((r) => r.status === 'PENDING')
              .map((r) => (
                <PendingWithdrawalRow key={r.id} row={r} />
              ))}
          </ul>
        )}
        <button
          type="button"
          className={`${btnGhost} mt-3`}
          onClick={() => {
            void pending.refetch();
          }}
        >
          Refresh
        </button>
      </div>
    </div>
  );
}
