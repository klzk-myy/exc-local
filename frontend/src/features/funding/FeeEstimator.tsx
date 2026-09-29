/**
 * Fee estimator — POST /api/v1/funding/fee-estimate (Phase-11 Task 11.3.9).
 * The route is registered but Status=Stub until Phase-11 lands the fee
 * schedule engine — a 501 degrades to "estimator unavailable" rather than
 * an error banner (the withdrawal form stays usable without it).
 */
import { useState } from 'react';
import { useMutation } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError } from '@/lib/api';
import { ErrorBox, Field, btnPrimary, inputCls, selectCls } from '@/lib/ui';

import * as api from './api';

const PRESETS = ['1000', '10000', '50000'] as const;
const DIRECTIONS = ['DEPOSIT', 'WITHDRAWAL', 'TRANSFER'] as const;

export default function FeeEstimator() {
  const [currency, setCurrency] = useState('USD');
  const [amount, setAmount] = useState('');
  const [rail, setRail] = useState<string>('SWIFT');
  const [direction, setDirection] = useState<string>('WITHDRAWAL');

  const mut = useMutation({
    mutationFn: () => api.feeEstimate(apiClient, { amount, currency, rail, direction }),
  });

  const unavailable =
    mut.error instanceof ApiError &&
    (mut.error.status === 501 || mut.error.code === 'NOT_IMPLEMENTED');
  const est = mut.data;

  return (
    <div className="rounded border border-neutral-800 p-3">
      <h4 className="mb-2 text-sm font-semibold">Fee &amp; arrival estimate</h4>
      <div className="grid grid-cols-2 gap-3">
        <Field label="Amount">
          {(id, describedBy, invalid) => (
            <input
              id={id}
              aria-describedby={describedBy}
              aria-invalid={invalid}
              className={inputCls}
              inputMode="decimal"
              value={amount}
              onChange={(e) => {
                setAmount(e.target.value);
              }}
            />
          )}
        </Field>
        <Field label="Currency">
          {(id, describedBy, invalid) => (
            <input
              id={id}
              aria-describedby={describedBy}
              aria-invalid={invalid}
              className={inputCls}
              maxLength={3}
              value={currency}
              onChange={(e) => {
                setCurrency(e.target.value.toUpperCase());
              }}
            />
          )}
        </Field>
        <Field label="Rail">
          {(id, describedBy, invalid) => (
            <select
              id={id}
              aria-describedby={describedBy}
              aria-invalid={invalid}
              className={selectCls}
              value={rail}
              onChange={(e) => {
                setRail(e.target.value);
              }}
            >
              {api.BANK_METHODS.map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </select>
          )}
        </Field>
        <Field label="Direction">
          {(id, describedBy, invalid) => (
            <select
              id={id}
              aria-describedby={describedBy}
              aria-invalid={invalid}
              className={selectCls}
              value={direction}
              onChange={(e) => {
                setDirection(e.target.value);
              }}
            >
              {DIRECTIONS.map((d) => (
                <option key={d} value={d}>
                  {d}
                </option>
              ))}
            </select>
          )}
        </Field>
      </div>
      <div className="mb-3 flex gap-2">
        {PRESETS.map((p) => (
          <button
            key={p}
            type="button"
            className="rounded border border-neutral-700 px-2 py-1 text-xs text-neutral-300 hover:bg-neutral-800"
            onClick={() => {
              setAmount(p);
            }}
          >
            {Number(p).toLocaleString()}
          </button>
        ))}
      </div>
      <button
        type="button"
        className={btnPrimary}
        disabled={mut.isPending || amount === '' || currency.length !== 3}
        onClick={() => {
          mut.mutate();
        }}
      >
        {mut.isPending ? 'Estimating…' : 'Estimate'}
      </button>

      {unavailable ? (
        <p className="mt-2 text-sm text-neutral-400" role="status">
          The fee estimator is not available yet — exact fees are confirmed on the withdrawal review
          screen before submission.
        </p>
      ) : (
        <ErrorBox error={mut.error} />
      )}
      {est !== undefined && (
        <dl className="mt-3 space-y-1 text-sm" data-testid="fee-estimate">
          {est.fee !== undefined && (
            <div className="flex justify-between">
              <dt className="text-neutral-400">Rail fee</dt>
              <dd>
                {est.fee} {est.fee_currency ?? est.currency}
              </dd>
            </div>
          )}
          {est.net_amount !== undefined && (
            <div className="flex justify-between">
              <dt className="text-neutral-400">Net amount</dt>
              <dd>
                {est.net_amount} {est.currency}
              </dd>
            </div>
          )}
          {est.estimated_arrival !== undefined && (
            <div className="flex justify-between">
              <dt className="text-neutral-400">Estimated arrival</dt>
              <dd>{est.estimated_arrival}</dd>
            </div>
          )}
          {est.cutoff_time !== undefined && (
            <div className="flex justify-between">
              <dt className="text-neutral-400">Same-day cut-off</dt>
              <dd>{est.cutoff_time}</dd>
            </div>
          )}
        </dl>
      )}
    </div>
  );
}
