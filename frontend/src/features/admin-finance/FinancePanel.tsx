/**
 * Finance statements panel (Phase-10.5 Task 10.5.3.10 §3) — the
 * trial-balance JSON view plus P&L / balance-sheet CSV-Parquet export
 * links, monthly invoice register, and the admin reporting-values
 * register (LEI / EMIR / MiFID reference values, Compliance-officer
 * write).
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
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import { fetchInvoices, fetchReportingValues, fetchTrialBalance, setReportingValue } from './api';

export function FinancePanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [tbDate, setTbDate] = useState('');
  const [tbCcy, setTbCcy] = useState('');
  const [invAccount, setInvAccount] = useState('');
  const [invMonth, setInvMonth] = useState('');
  const [rvScope, setRvScope] = useState('');
  const [rvForm, setRvForm] = useState({ scope: '', key: '', value: '', source: '' });
  const [notice, setNotice] = useState<string | null>(null);

  const tb = useQuery({
    queryKey: ['admin-trial-balance', tbDate, tbCcy],
    queryFn: () =>
      fetchTrialBalance(adminApi, {
        date: tbDate === '' ? undefined : tbDate,
        currency: tbCcy === '' ? undefined : tbCcy,
      }),
  });
  const invoices = useQuery({
    queryKey: ['admin-invoices', invAccount, invMonth],
    queryFn: () =>
      fetchInvoices(adminApi, {
        account: Number(invAccount) || undefined,
        month: invMonth === '' ? undefined : invMonth,
      }),
  });
  const values = useQuery({
    queryKey: ['admin-reporting-values', rvScope],
    queryFn: () => fetchReportingValues(adminApi, rvScope === '' ? undefined : rvScope),
  });
  const setRv = useMutation({
    mutationFn: () =>
      setReportingValue(adminApi, {
        scope: rvForm.scope,
        key: rvForm.key,
        value: rvForm.value,
        source: rvForm.source,
      }),
    onSuccess: () => {
      setNotice('Reporting value upserted (audit-logged).');
      void qc.invalidateQueries({ queryKey: ['admin-reporting-values'] });
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Set failed'),
  });

  const denied =
    (tb.error !== null && isAccessDenied(tb.error)) ||
    (invoices.error !== null && isAccessDenied(invoices.error)) ||
    (values.error !== null && isAccessDenied(values.error));
  if (denied) return <AccessDeniedCard />;

  return (
    <section className={cardCls} aria-label="Finance statements">
      <h2 className="mb-2 text-sm font-semibold">Finance statements</h2>

      <div className="mb-2 flex flex-wrap items-end gap-2">
        <input
          aria-label="Business date"
          className={inputCls}
          type="date"
          value={tbDate}
          onChange={(e) => {
            setTbDate(e.target.value);
          }}
        />
        <input
          aria-label="Currency filter"
          className={inputCls}
          placeholder="currency"
          value={tbCcy}
          onChange={(e) => {
            setTbCcy(e.target.value);
          }}
        />
        <a
          className={btnGhost}
          href={`/api/v1/admin/finance/pnl?format=csv${tbDate !== '' ? `&from=${tbDate}` : ''}`}
        >
          P&amp;L CSV
        </a>
        <a
          className={btnGhost}
          href={`/api/v1/admin/finance/balance-sheet?format=csv${tbDate !== '' ? `&date=${tbDate}` : ''}`}
        >
          Balance sheet CSV
        </a>
      </div>
      {tb.error !== null ? <ErrorBox error={tb.error} /> : null}
      {tb.data !== undefined ? (
        <>
          <p className={hintTextCls}>
            Trial balance — business date {tb.data.businessDate.slice(0, 10)} · generated{' '}
            {tb.data.generatedAt.slice(0, 16)}
          </p>
          {tb.data.currencies.length === 0 ? (
            <p className="text-sm text-neutral-500">No GL entries for the day yet.</p>
          ) : null}
          {tb.data.currencies.map((c) => (
            <div key={c.currency} className="mt-2 overflow-x-auto">
              <table className={tableCls}>
                <caption className="text-left text-xs font-semibold">{c.currency}</caption>
                <thead>
                  <tr>
                    <th className={thCls}>Debits</th>
                    <th className={thCls}>Credits</th>
                    <th className={thCls}>Assets</th>
                    <th className={thCls}>Liabilities</th>
                    <th className={thCls}>Equity</th>
                    <th className={thCls}>Revenue</th>
                    <th className={thCls}>Expenses</th>
                    <th className={thCls}>Net income</th>
                  </tr>
                </thead>
                <tbody>
                  <tr>
                    <td className={tdCls}>{c.totalDebits}</td>
                    <td className={tdCls}>{c.totalCredits}</td>
                    <td className={tdCls}>{c.assets}</td>
                    <td className={tdCls}>{c.liabilities}</td>
                    <td className={tdCls}>{c.equity}</td>
                    <td className={tdCls}>{c.revenue}</td>
                    <td className={tdCls}>{c.expenses}</td>
                    <td className={tdCls}>{c.netIncome}</td>
                  </tr>
                </tbody>
              </table>
            </div>
          ))}
        </>
      ) : null}

      <div className="mt-4 flex flex-wrap items-end gap-2">
        <span className="text-sm font-semibold">Monthly invoices</span>
        <input
          aria-label="Account filter"
          className={inputCls}
          placeholder="account"
          value={invAccount}
          onChange={(e) => {
            setInvAccount(e.target.value);
          }}
        />
        <input
          aria-label="Invoice month"
          className={inputCls}
          type="month"
          value={invMonth}
          onChange={(e) => {
            setInvMonth(e.target.value);
          }}
        />
      </div>
      {invoices.error !== null ? <ErrorBox error={invoices.error} /> : null}
      {invoices.data?.length === 0 ? (
        <p className="text-sm text-neutral-500">No invoices for the filter.</p>
      ) : null}
      {invoices.data !== undefined && invoices.data.length > 0 ? (
        <table className={`${tableCls} mt-1`}>
          <thead>
            <tr>
              <th className={thCls}>Invoice</th>
              <th className={thCls}>Account</th>
              <th className={thCls}>Month</th>
              <th className={thCls}>CCY</th>
              <th className={thCls}>Trading</th>
              <th className={thCls}>MM rebates</th>
              <th className={thCls}>Connectivity</th>
              <th className={thCls}>Total</th>
              <th className={thCls}>Status</th>
            </tr>
          </thead>
          <tbody>
            {invoices.data.map((i) => (
              <tr key={i.invoiceId}>
                <td className={tdCls}>{i.invoiceId}</td>
                <td className={tdCls}>{i.accountId}</td>
                <td className={tdCls}>{i.month.slice(0, 7)}</td>
                <td className={tdCls}>{i.currency}</td>
                <td className={tdCls}>{i.tradingFees}</td>
                <td className={tdCls}>{i.mmRebates}</td>
                <td className={tdCls}>{i.connectivityFees}</td>
                <td className={tdCls}>{i.total}</td>
                <td className={tdCls}>
                  <StatusBadge value={i.status || 'UNKNOWN'} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}

      <div className="mt-4 flex flex-wrap items-end gap-2">
        <span className="text-sm font-semibold">Reporting values</span>
        <input
          aria-label="Scope filter"
          className={inputCls}
          placeholder="scope filter"
          value={rvScope}
          onChange={(e) => {
            setRvScope(e.target.value);
          }}
        />
      </div>
      {values.error !== null ? <ErrorBox error={values.error} /> : null}
      {values.data !== undefined && values.data.length > 0 ? (
        <table className={`${tableCls} mt-1`}>
          <thead>
            <tr>
              <th className={thCls}>Scope</th>
              <th className={thCls}>Key</th>
              <th className={thCls}>Value</th>
              <th className={thCls}>Source</th>
              <th className={thCls}>Effective</th>
            </tr>
          </thead>
          <tbody>
            {values.data.map((v) => (
              <tr key={v.id}>
                <td className={tdCls}>{v.scope}</td>
                <td className={tdCls}>{v.key}</td>
                <td className={`${tdCls} font-mono text-xs`}>
                  {typeof v.value === 'string' ? v.value : JSON.stringify(v.value)}
                </td>
                <td className={tdCls}>{v.source}</td>
                <td className={tdCls}>{v.effectiveFrom.slice(0, 10)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
      <form
        aria-label="Set reporting value"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (rvForm.scope !== '' && rvForm.key !== '' && rvForm.source !== '') {
            setRv.mutate();
          }
        }}
      >
        {(
          [
            ['scope', 'scope'],
            ['key', 'key'],
            ['value', 'value (JSON or scalar)'],
            ['source', 'source'],
          ] as const
        ).map(([k, ph]) => (
          <input
            key={k}
            aria-label={`Reporting ${k}`}
            className={inputCls}
            placeholder={ph}
            value={rvForm[k]}
            onChange={(e) => {
              setRvForm({ ...rvForm, [k]: e.target.value });
            }}
          />
        ))}
        <button type="submit" className={btnPrimary} disabled={setRv.isPending}>
          Upsert (Compliance)
        </button>
      </form>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
