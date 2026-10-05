/**
 * Deposit tab — GET /api/v1/deposits/{currency} (live). Renders the
 * account-scoped payment reference + nostro bank instructions with
 * copy-to-clipboard, an EPC QR for SEPA-style IBAN instructions, and
 * pending deposits from the unified funding feed.
 */
import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import {
  CopyButton,
  ErrorBox,
  Field,
  QrBlock,
  StatusBadge,
  btnPrimary,
  cardCls,
  inputCls,
  selectCls,
} from '@/lib/ui';

import * as api from './api';

const FIAT = ['USD', 'EUR', 'GBP', 'CHF', 'JPY', 'CAD', 'AUD', 'SEK', 'NOK'] as const;

/** EPC069-12 SEPA credit-transfer QR payload (scannable by EU banking
 * apps). Only emitted when the instruction carries an IBAN + BIC. */
function epcPayload(ins: api.NostroAccount, reference: string, currency: string): string | null {
  if (ins.iban === undefined || ins.iban === '' || ins.bank_code === undefined) return null;
  const lines = [
    'BCD',
    '002',
    '1',
    'SCT',
    ins.bank_code,
    ins.beneficiary_name ?? 'Exchange',
    ins.iban.replaceAll(' ', ''),
    '',
    '',
    reference,
    `Deposit ${currency}`,
  ];
  return lines.join('\n');
}

function InstructionCard({
  ins,
  reference,
  currency,
}: {
  ins: api.NostroAccount;
  reference: string;
  currency: string;
}) {
  const epc = epcPayload(ins, reference, currency);
  const rows: [string, string | undefined][] = [
    ['Bank', ins.bank_name],
    ['SWIFT/BIC', ins.bank_code],
    ['IBAN', ins.iban],
    ['Account number', ins.account_number],
    ['Beneficiary', ins.beneficiary_name],
    ['Rail', ins.rail],
  ];
  return (
    <div className="rounded border border-neutral-800 p-3">
      <div className="mb-2 flex items-center justify-between">
        <span className="text-sm font-medium">{ins.bank_name}</span>
        <StatusBadge value={ins.status} />
      </div>
      <dl className="space-y-1 text-sm">
        {rows
          .filter(([, v]) => v !== undefined && v !== '')
          .map(([k, v]) => (
            <div key={k} className="flex items-center justify-between gap-2">
              <dt className="text-neutral-400">{k}</dt>
              <dd className="flex items-center gap-1 font-mono text-xs text-neutral-200">
                {v}
                <CopyButton text={v ?? ''} />
              </dd>
            </div>
          ))}
      </dl>
      {epc !== null && (
        <div className="mt-3">
          <QrBlock value={epc} size={140} label="SEPA payment QR" />
          <p className="mt-1 text-xs text-neutral-500">
            Scan with a EU banking app — prefills beneficiary, IBAN and reference.
          </p>
        </div>
      )}
    </div>
  );
}

/** Client-declared inbound wire — POST /deposits (Task 11.3.3). The
 * detection poller adopts the intent by reference; Idempotency-Key is
 * generated per submission so replays return the stored row. */
function DepositIntentForm({ currency }: { currency: string }) {
  const qc = useQueryClient();
  const [amount, setAmount] = useState('');
  const [method, setMethod] = useState<string>('SWIFT');
  const intent = useMutation({
    mutationFn: () =>
      api.createDepositIntent(apiClient, {
        currency,
        amount,
        bankMethod: method,
      }),
    onSuccess: async () => {
      setAmount('');
      await qc.invalidateQueries({ queryKey: ['funding', 'pending-deposits'] });
    },
  });

  return (
    <div className={cardCls}>
      <h2 className="mb-1 text-sm font-semibold">Declare an incoming wire</h2>
      <p className="mb-3 text-xs text-neutral-400">
        Registering your transfer helps settlement reconciliation attribute it — the deposit stays
        PENDING until the bank credit is detected.
      </p>
      <form
        className="grid gap-3 sm:grid-cols-3"
        onSubmit={(e) => {
          e.preventDefault();
          intent.mutate();
        }}
      >
        <Field label={`Amount (${currency})`} required>
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
        <Field label="Bank method">
          {(id) => (
            <select
              id={id}
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
        <div className="flex items-end">
          <button type="submit" className={btnPrimary} disabled={intent.isPending || amount === ''}>
            {intent.isPending ? 'Registering…' : 'Register deposit'}
          </button>
        </div>
      </form>
      <ErrorBox error={intent.error} />
      {intent.isSuccess && (
        <p className="mt-2 text-sm text-emerald-400" role="status">
          Deposit #{intent.data.deposit_id}{' '}
          {intent.data.replayed === true ? 'already registered' : 'registered'}— status{' '}
          {intent.data.status}
          {intent.data.flags !== undefined &&
            intent.data.flags.length > 0 &&
            ` · flags: ${intent.data.flags.join(', ')}`}
        </p>
      )}
    </div>
  );
}

export default function DepositPanel() {
  const [currency, setCurrency] = useState('USD');
  const instructions = useQuery({
    queryKey: ['deposits', currency],
    queryFn: () => api.depositInstructions(apiClient, currency),
    retry: false,
  });
  const pending = useQuery({
    queryKey: ['funding', 'pending-deposits'],
    queryFn: () => api.fundingHistory(apiClient, { type: 'DEPOSIT', status: 'PENDING', limit: 20 }),
  });

  return (
    <div className="space-y-4">
      <div className={cardCls}>
        <Field label="Currency">
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
              {FIAT.map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </select>
          )}
        </Field>

        {instructions.isError && <ErrorBox error={instructions.error} />}
        {instructions.isPending && <p className="text-sm text-neutral-400">Loading…</p>}
        {instructions.data !== undefined && (
          <>
            <div className="mb-3 rounded border border-sky-800/60 bg-sky-950/30 p-3">
              <p className="text-xs text-neutral-400">Payment reference — quote verbatim:</p>
              <p className="flex items-center gap-2 font-mono text-sm text-sky-300">
                {instructions.data.reference}
                <CopyButton text={instructions.data.reference} label="Copy reference" />
              </p>
            </div>
            <div className="space-y-3">
              {instructions.data.instructions.map((ins) => (
                <InstructionCard
                  key={ins.id}
                  ins={ins}
                  reference={instructions.data.reference}
                  currency={instructions.data.currency}
                />
              ))}
              {instructions.data.instructions.length === 0 && (
                <p className="text-sm text-neutral-400">No deposit instructions for {currency}.</p>
              )}
            </div>
          </>
        )}
      </div>

      <DepositIntentForm currency={currency} />

      <div className={cardCls}>
        <h2 className="mb-2 text-sm font-semibold">Pending deposits</h2>
        {(pending.data?.data ?? []).filter((r) => r.status === 'PENDING').length === 0 ? (
          <p className="text-sm text-neutral-400">No deposits awaiting settlement.</p>
        ) : (
          <ul className="space-y-1 text-sm">
            {pending.data?.data
              .filter((r) => r.status === 'PENDING')
              .map((r) => (
                <li key={r.id} className="flex justify-between">
                  <span>
                    {r.amount} {r.currency}
                  </span>
                  <StatusBadge value={r.status} />
                </li>
              ))}
          </ul>
        )}
      </div>
    </div>
  );
}
