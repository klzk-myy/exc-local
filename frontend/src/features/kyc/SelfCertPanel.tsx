/**
 * Investor self-certification (Task 10.5.3.27 gate-coverage wiring,
 * Phase-14 Task 14.3.7 client categorisation) — income/net-worth/
 * experience declaration plus explicit risk acknowledgement. GET
 * renders the stored declaration; POST submits a new one.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';

import { apiClient } from '@/app/runtime';
import { ErrorBox, btnPrimary, cardCls, inputCls, labelCls } from '@/lib/ui';

import * as api from './api';

const EXPERIENCE = ['none', 'lt-1y', '1-3y', 'gt-3y'] as const;

export function SelfCertPanel() {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ['kyc', 'self-cert'],
    queryFn: () => api.fetchSelfCert(apiClient),
    retry: false,
  });
  const [form, setForm] = useState({
    annual_income: '',
    net_worth: '',
    trading_experience: 'none',
    acknowledges_risk: false,
  });
  const [notice, setNotice] = useState<string | null>(null);

  useEffect(() => {
    const c = q.data;
    if (c === null || c === undefined) return;
    setForm({
      annual_income: c.annualIncome ?? '',
      net_worth: c.netWorth ?? '',
      trading_experience: c.tradingExperience ?? 'none',
      acknowledges_risk: c.acknowledgesRisk === true,
    });
  }, [q.data]);

  const submit = useMutation({
    mutationFn: () =>
      api.postSelfCert(apiClient, {
        annual_income: form.annual_income,
        net_worth: form.net_worth,
        trading_experience: form.trading_experience,
        acknowledges_risk: form.acknowledges_risk,
      }),
    onSuccess: async () => {
      setNotice('Self-certification recorded');
      await qc.invalidateQueries({ queryKey: ['kyc', 'self-cert'] });
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Submission failed'),
  });

  return (
    <section className={cardCls} aria-label="Self-certification">
      <h2 className="mb-1 text-sm font-semibold">Investor self-certification</h2>
      <p className="mb-3 text-sm text-neutral-400">
        Declares financial situation and experience for client categorisation (appropriateness).
        Stored declaration is shown below when present.
      </p>
      {notice !== null && (
        <p role="status" className="mb-2 text-xs text-sky-300">
          {notice}
        </p>
      )}
      {q.isError && <ErrorBox error={q.error} />}
      {q.data?.submittedAt !== undefined && (
        <p className="mb-2 text-xs text-neutral-500">Last submitted: {q.data.submittedAt}</p>
      )}
      <form
        className="grid gap-3 sm:grid-cols-2"
        onSubmit={(e) => {
          e.preventDefault();
          submit.mutate();
        }}
      >
        <label className={labelCls}>
          Annual income
          <input
            className={inputCls}
            value={form.annual_income}
            onChange={(e) => setForm({ ...form, annual_income: e.target.value })}
            required
            placeholder="e.g. 50000"
          />
        </label>
        <label className={labelCls}>
          Net worth
          <input
            className={inputCls}
            value={form.net_worth}
            onChange={(e) => setForm({ ...form, net_worth: e.target.value })}
            required
            placeholder="e.g. 100000"
          />
        </label>
        <label className={labelCls}>
          Trading experience
          <select
            className={inputCls}
            value={form.trading_experience}
            onChange={(e) => setForm({ ...form, trading_experience: e.target.value })}
          >
            {EXPERIENCE.map((x) => (
              <option key={x} value={x}>
                {x}
              </option>
            ))}
          </select>
        </label>
        <label className="flex items-center gap-2 text-sm text-neutral-300">
          <input
            type="checkbox"
            checked={form.acknowledges_risk}
            onChange={(e) => setForm({ ...form, acknowledges_risk: e.target.checked })}
            required
          />
          I acknowledge the risks of leveraged FX trading
        </label>
        <div className="sm:col-span-2">
          <button type="submit" className={btnPrimary} disabled={submit.isPending}>
            {submit.isPending ? 'Submitting…' : 'Submit self-certification'}
          </button>
        </div>
      </form>
    </section>
  );
}
