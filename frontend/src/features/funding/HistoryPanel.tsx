/**
 * Unified funding history — GET /api/v1/funding (Task 10.3.23 item 5).
 * Cursor-paginated envelope {data, next_cursor, limit, total} with
 * server-side filters (type, currency, status, from, to).
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
  inputCls,
  selectCls,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import * as api from './api';
import FeeEstimator from './FeeEstimator';

const FIAT = ['USD', 'EUR', 'GBP', 'CHF', 'JPY', 'CAD', 'AUD', 'SEK', 'NOK'] as const;

/** Currency conversion — POST /funding/convert + GET /funding/conversions
 * (Task 11.3.9). Rate source is fail-closed server-side; mid/spread/
 * applied rate are rendered verbatim from the persisted record. */
function ConvertCard() {
  const qc = useQueryClient();
  const [from, setFrom] = useState('USD');
  const [to, setTo] = useState('EUR');
  const [amount, setAmount] = useState('');
  const history = useQuery({
    queryKey: ['funding', 'conversions'],
    queryFn: () => api.conversionHistory(apiClient),
  });
  const convert = useMutation({
    mutationFn: () => api.convertFunds(apiClient, { fromCurrency: from, toCurrency: to, amount }),
    onSuccess: async () => {
      setAmount('');
      await qc.invalidateQueries({ queryKey: ['funding', 'conversions'] });
      await qc.invalidateQueries({ queryKey: ['account', 'balances'] });
    },
  });

  return (
    <div className={cardCls}>
      <h2 className="mb-2 text-sm font-semibold">Currency conversion</h2>
      <form
        className="grid gap-3 sm:grid-cols-4"
        onSubmit={(e) => {
          e.preventDefault();
          convert.mutate();
        }}
      >
        <Field label="From">
          {(id) => (
            <select
              id={id}
              className={selectCls}
              value={from}
              onChange={(e) => {
                setFrom(e.target.value);
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
        <Field label="To">
          {(id) => (
            <select
              id={id}
              className={selectCls}
              value={to}
              onChange={(e) => {
                setTo(e.target.value);
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
        <Field label="Convert amount" required>
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
        <div className="flex items-end">
          <button
            type="submit"
            className={btnPrimary}
            disabled={convert.isPending || amount === '' || from === to}
          >
            {convert.isPending ? 'Converting…' : 'Convert'}
          </button>
        </div>
      </form>
      <ErrorBox error={convert.error} />
      {convert.isSuccess && (
        <p className="mt-2 text-sm text-emerald-400" role="status">
          {convert.data.converted === true
            ? `Converted ${convert.data.amount_from ?? ''} ${convert.data.from_currency ?? ''} → ${
                convert.data.amount_to ?? ''
              } ${convert.data.to_currency ?? ''} @ ${convert.data.rate_applied ?? ''}`
            : 'Conversion not executed.'}
        </p>
      )}
      {history.isError && <ErrorBox error={history.error} />}
      {(history.data ?? []).length > 0 && (
        <table className={`${tableCls} mt-3`}>
          <thead>
            <tr>
              <th className={thCls}>When</th>
              <th className={thCls}>From</th>
              <th className={thCls}>To</th>
              <th className={thCls}>Rate</th>
              <th className={thCls}>Source</th>
            </tr>
          </thead>
          <tbody>
            {(history.data ?? []).map((c) => (
              <tr key={c.id}>
                <td className={tdCls}>
                  {c.created_at !== undefined ? new Date(c.created_at).toLocaleString() : '—'}
                </td>
                <td className={tdCls}>
                  {c.amount_from} {c.from_currency}
                </td>
                <td className={tdCls}>
                  {c.amount_to} {c.to_currency}
                </td>
                <td className={tdCls}>
                  {c.rate_applied}
                  <span className="ml-1 text-xs text-neutral-500">
                    (mid {c.mid_rate} · {c.spread_bps}bps)
                  </span>
                </td>
                <td className={tdCls}>{c.rate_source ?? '—'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

export default function HistoryPanel() {
  const [type, setType] = useState('');
  const [currency, setCurrency] = useState('');
  const [status, setStatus] = useState('');
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [stack, setStack] = useState<api.FundingTxRow[]>([]);

  const q = useQuery({
    queryKey: ['funding', 'history', type, currency, status, cursor],
    queryFn: () => api.fundingHistory(apiClient, { type, currency, status, limit: 50, cursor }),
    // Merge cursor pages — each refetch appends the new page.
    placeholderData: (prev) => prev,
  });
  const rows = cursor === undefined ? (q.data?.data ?? []) : [...stack, ...(q.data?.data ?? [])];

  /** Filters refetch via the queryKey — they must also reset pagination. */
  const resetPages = () => {
    setCursor(undefined);
    setStack([]);
  };

  return (
    <div className="space-y-4">
      <FeeEstimator />
      <ConvertCard />
      <div className={cardCls}>
        <h2 className="mb-2 text-sm font-semibold">Funding history</h2>
        <div className="mb-3 grid grid-cols-3 gap-3">
          <Field label="Type">
            {(id, describedBy, invalid) => (
              <select
                id={id}
                aria-describedby={describedBy}
                aria-invalid={invalid}
                className={selectCls}
                value={type}
                onChange={(e) => {
                  resetPages();
                  setType(e.target.value);
                }}
              >
                <option value="">All</option>
                {api.FUNDING_TYPES.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
              </select>
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
                placeholder="USD"
                value={currency}
                onChange={(e) => {
                  resetPages();
                  setCurrency(e.target.value.toUpperCase());
                }}
              />
            )}
          </Field>
          <Field label="Status">
            {(id, describedBy, invalid) => (
              <select
                id={id}
                aria-describedby={describedBy}
                aria-invalid={invalid}
                className={selectCls}
                value={status}
                onChange={(e) => {
                  resetPages();
                  setStatus(e.target.value);
                }}
              >
                <option value="">All</option>
                {api.FUNDING_STATUSES.map((s) => (
                  <option key={s} value={s}>
                    {s}
                  </option>
                ))}
              </select>
            )}
          </Field>
        </div>
        <ErrorBox error={q.error} />
        {q.isPending ? (
          <p className="text-sm text-neutral-400">Loading…</p>
        ) : rows.length === 0 ? (
          <p className="text-sm text-neutral-400">No funding activity matches.</p>
        ) : (
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>When</th>
                <th className={thCls}>Type</th>
                <th className={thCls}>Amount</th>
                <th className={thCls}>Method</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Review</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={`${r.type}-${r.id}`}>
                  <td className={tdCls}>{new Date(r.created_at).toLocaleString()}</td>
                  <td className={tdCls}>{r.type}</td>
                  <td className={tdCls}>
                    {r.amount} {r.currency}
                  </td>
                  <td className={tdCls}>{r.bank_method ?? '—'}</td>
                  <td className={tdCls}>
                    <StatusBadge value={r.status} />
                  </td>
                  <td className={tdCls}>
                    {r.review_tier !== undefined ? <StatusBadge value={r.review_tier} /> : '—'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {q.data !== undefined && q.data.nextCursor !== '' && (
          <button
            type="button"
            className={`${btnGhost} mt-3`}
            onClick={() => {
              setStack(rows);
              setCursor(q.data.nextCursor);
            }}
          >
            Load more
          </button>
        )}
        <p className="mt-2 text-xs text-neutral-500">
          {rows.length} of {q.data?.total ?? rows.length} entries · filters apply server-side
        </p>
      </div>
    </div>
  );
}
