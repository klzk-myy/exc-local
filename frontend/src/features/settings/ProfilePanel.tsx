/**
 * Profile panel — GET/PUT /account/profile (Task 10.3.22 item 1).
 * MiFID categorization + KYC tier render read-only (server-owned).
 */
import { useEffect, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ErrorBox, Field, StatusBadge, btnPrimary, cardCls, inputCls } from '@/lib/ui';

import * as api from './api';

export default function ProfilePanel() {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ['account', 'profile'],
    queryFn: () => api.getProfile(apiClient),
  });
  const [displayName, setDisplayName] = useState('');
  const [phone, setPhone] = useState('');
  const [taxResidency, setTaxResidency] = useState('');
  const [saved, setSaved] = useState(false);

  const p = q.data;
  useEffect(() => {
    if (p === undefined) return;
    setDisplayName(p.display_name ?? '');
    setPhone(p.phone ?? '');
    setTaxResidency(p.tax_residency ?? '');
  }, [p]);

  const mut = useMutation({
    mutationFn: () =>
      api.putProfile(apiClient, {
        display_name: displayName,
        phone,
        tax_residency: taxResidency,
      }),
    onSuccess: async () => {
      setSaved(true);
      await qc.invalidateQueries({ queryKey: ['account', 'profile'] });
    },
  });

  if (q.isPending) return <p className="text-sm text-neutral-400">Loading profile…</p>;

  return (
    <div className={cardCls}>
      {q.isError && (
        <ErrorBox
          error={q.error}
          onDismiss={() => {
            void q.refetch();
          }}
        />
      )}
      <div className="mb-4 flex items-center justify-between">
        <div>
          <h2 className="text-lg font-medium">Profile</h2>
          <p className="text-sm text-neutral-400">{p?.email ?? '—'}</p>
        </div>
        <div className="flex gap-2">
          {p?.kyc_tier !== undefined && <StatusBadge value={p.kyc_tier} />}
          {p?.mifid_category !== undefined && <StatusBadge value={`MIFID_${p.mifid_category}`} />}
        </div>
      </div>
      {saved && !mut.isPending && (
        <p className="mb-3 text-sm text-emerald-400" role="status">
          Profile saved.
        </p>
      )}
      <ErrorBox
        error={mut.error}
        onDismiss={() => {
          mut.reset();
        }}
      />
      <form
        onSubmit={(e) => {
          e.preventDefault();
          setSaved(false);
          mut.mutate();
        }}
      >
        <Field label="Display name">
          {(id, describedBy, invalid) => (
            <input
              id={id}
              aria-describedby={describedBy}
              aria-invalid={invalid}
              className={inputCls}
              autoComplete="name"
              value={displayName}
              onChange={(e) => {
                setDisplayName(e.target.value);
              }}
            />
          )}
        </Field>
        <Field label="Phone">
          {(id, describedBy, invalid) => (
            <input
              id={id}
              aria-describedby={describedBy}
              aria-invalid={invalid}
              className={inputCls}
              type="tel"
              autoComplete="tel"
              value={phone}
              onChange={(e) => {
                setPhone(e.target.value);
              }}
            />
          )}
        </Field>
        <Field
          label="Tax residency"
          hint="ISO country code of your tax residence — feeds the tax-report inputs."
        >
          {(id, describedBy, invalid) => (
            <input
              id={id}
              aria-describedby={describedBy}
              aria-invalid={invalid}
              className={inputCls}
              maxLength={2}
              value={taxResidency}
              onChange={(e) => {
                setTaxResidency(e.target.value.toUpperCase());
              }}
            />
          )}
        </Field>
        {p?.mifid_category !== undefined && (
          <p className="mb-4 text-sm text-neutral-400">
            MiFID client categorization:{' '}
            <strong className="text-neutral-200">{p.mifid_category}</strong> (set by the venue —
            contact support to request a re-categorization review).
          </p>
        )}
        <button type="submit" className={btnPrimary} disabled={mut.isPending}>
          {mut.isPending ? 'Saving…' : 'Save profile'}
        </button>
      </form>
    </div>
  );
}
