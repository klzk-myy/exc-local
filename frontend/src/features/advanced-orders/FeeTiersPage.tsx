/**
 * Admin fee-tier assignment (Task 10.3.7 item 3) — Compliance Officer /
 * Finance Ops assign a bespoke product profile / fee tier to an
 * institutional account via PUT /api/v1/admin/accounts/{id}/product-profile
 * (Phase-14 Task 14.3.7).
 *
 * The endpoint is live (Phase-03 fee schedule); an error surfaces
 * honestly via ErrorBox rather than pretending success.
 */
import { useMutation } from '@tanstack/react-query';
import { useState, type FormEvent } from 'react';

import { apiClient } from '@/app/runtime';
import { ErrorBox, btnPrimary, inputCls, labelCls } from '@/lib/ui';
import { assignProductProfile, type Api } from '@/lib/trading/api';

export default function FeeTiersPage({ api = apiClient }: { api?: Api }) {
  const [accountId, setAccountId] = useState('');
  const [tier, setTier] = useState('');
  const [category, setCategory] = useState('');
  const [done, setDone] = useState<string | null>(null);

  const assign = useMutation({
    mutationFn: () =>
      assignProductProfile(
        accountId.trim(),
        {
          category: category || undefined,
          fee_tier_id: tier || undefined,
        },
        api,
      ),
    onSuccess: () => {
      setDone(`Product profile submitted for account ${accountId}`);
    },
  });

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    setDone(null);
    assign.mutate();
  };

  return (
    <div className="mx-auto max-w-xl p-6">
      <h1 className="mb-1 text-xl font-semibold">Fee tier assignment</h1>
      <p className="mb-4 text-sm text-neutral-400">
        Assign a bespoke fee tier / product profile to an institutional account. Requires the
        Compliance Officer role — dual-control apply happens server-side.
      </p>

      <form onSubmit={onSubmit} className="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
        <div className="mb-4">
          <label htmlFor="ft-account" className={labelCls}>
            Account ID
          </label>
          <input
            id="ft-account"
            inputMode="numeric"
            required
            value={accountId}
            onChange={(e) => setAccountId(e.target.value)}
            className={inputCls}
            placeholder="institutional account id"
          />
        </div>
        <div className="mb-4">
          <label htmlFor="ft-tier" className={labelCls}>
            Fee tier ID
          </label>
          <input
            id="ft-tier"
            required
            value={tier}
            onChange={(e) => setTier(e.target.value)}
            className={inputCls}
            placeholder="e.g. INST_VIP_2"
          />
        </div>
        <div className="mb-4">
          <label htmlFor="ft-category" className={labelCls}>
            Client categorization
          </label>
          <select
            id="ft-category"
            value={category}
            onChange={(e) => setCategory(e.target.value)}
            className={inputCls}
          >
            <option value="">— unchanged —</option>
            <option value="RETAIL">Retail</option>
            <option value="PROFESSIONAL">Professional</option>
            <option value="ELIGIBLE_COUNTERPARTY">Eligible counterparty</option>
          </select>
        </div>

        <ErrorBox error={assign.isError ? assign.error : null} onDismiss={() => assign.reset()} />
        {done !== null && (
          <p className="mb-3 text-sm text-emerald-400" role="status">
            {done}
          </p>
        )}
        <button type="submit" className={`${btnPrimary} w-full`} disabled={assign.isPending}>
          {assign.isPending ? 'Assigning…' : 'Assign fee tier'}
        </button>
      </form>
    </div>
  );
}
