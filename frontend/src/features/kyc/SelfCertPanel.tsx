/**
 * Tax self-certification (Task 10.5.3.27 gate-coverage wiring,
 * compliance/taxcerts.go) — IRS W-8BEN / W-8BEN-E / W-9 intake. GET
 * renders the certification history; POST submits one form.
 * W-9 requires a TIN + tin_country "US"; W-8* TIN is optional.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import { apiClient } from '@/app/runtime';
import {
  ErrorBox,
  StatusBadge,
  btnPrimary,
  cardCls,
  inputCls,
  labelCls,
  tdCls,
  thCls,
  tableCls,
} from '@/lib/ui';

import * as api from './api';

const FORM_DESCRIPTIONS: Record<api.SelfCertFormType, string> = {
  'W-8BEN': 'Certificate of foreign status of beneficial owner (individual)',
  'W-8BEN-E': 'Certificate of status of beneficial owner (entity)',
  'W-9': 'Request for taxpayer identification number (US persons)',
};

export function SelfCertPanel() {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ['kyc', 'self-cert'],
    queryFn: () => api.fetchSelfCerts(apiClient),
    retry: false,
  });
  const [form, setForm] = useState({
    form_type: 'W-8BEN' as api.SelfCertFormType,
    legal_name: '',
    tin: '',
    tin_country: '',
    tin_kind: '',
  });
  const [notice, setNotice] = useState<string | null>(null);
  const isW9 = form.form_type === 'W-9';

  const submit = useMutation({
    mutationFn: () =>
      api.postSelfCert(apiClient, {
        form_type: form.form_type,
        legal_name: form.legal_name,
        tin: form.tin,
        tin_country: form.tin_country,
        tin_kind: form.tin_kind,
      }),
    onSuccess: async () => {
      setNotice('Certification recorded');
      setForm({ ...form, legal_name: '', tin: '', tin_country: '', tin_kind: '' });
      await qc.invalidateQueries({ queryKey: ['kyc', 'self-cert'] });
    },
  });

  return (
    <section className={cardCls} aria-label="Tax self-certification">
      <h2 className="mb-1 text-sm font-semibold">Tax self-certification</h2>
      <p className="mb-3 text-sm text-neutral-400">
        IRS self-certification (W-8BEN / W-8BEN-E / W-9) required for tax reporting. Submitting a
        new form supersedes the previous one.
      </p>
      {notice !== null && (
        <p role="status" className="mb-2 text-xs text-sky-300">
          {notice}
        </p>
      )}
      {q.isError && <ErrorBox error={q.error} />}
      {submit.isError && <ErrorBox error={submit.error} />}
      {(q.data?.length ?? 0) > 0 && (
        <table className={`${tableCls} mb-3`} aria-label="Certification history">
          <thead>
            <tr>
              <th className={thCls}>Form</th>
              <th className={thCls}>Legal name</th>
              <th className={thCls}>Status</th>
              <th className={thCls}>Submitted</th>
            </tr>
          </thead>
          <tbody>
            {(q.data ?? []).map((c) => (
              <tr key={c.id}>
                <td className={tdCls}>
                  {c.formType}
                  {c.supersededBy !== undefined ? (
                    <span className="ml-1 text-neutral-500">(superseded)</span>
                  ) : null}
                </td>
                <td className={tdCls}>{c.legalName ?? '—'}</td>
                <td className={tdCls}>
                  <StatusBadge value={c.status} />
                </td>
                <td className={tdCls}>{c.createdAt ?? '—'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <form
        className="grid gap-3 sm:grid-cols-2"
        onSubmit={(e) => {
          e.preventDefault();
          submit.mutate();
        }}
      >
        <label className={labelCls}>
          Form type
          <select
            className={inputCls}
            value={form.form_type}
            onChange={(e) =>
              setForm({ ...form, form_type: e.target.value as api.SelfCertFormType })
            }
          >
            {api.SELF_CERT_FORMS.map((f) => (
              <option key={f} value={f}>
                {f}
              </option>
            ))}
          </select>
          <span className="mt-1 block text-xs text-neutral-500">
            {FORM_DESCRIPTIONS[form.form_type]}
          </span>
        </label>
        <label className={labelCls}>
          Legal name
          <input
            className={inputCls}
            value={form.legal_name}
            onChange={(e) => setForm({ ...form, legal_name: e.target.value })}
            required
            placeholder="As on the certification"
          />
        </label>
        <label className={labelCls}>
          {isW9 ? 'TIN (required)' : 'TIN (optional)'}
          <input
            className={inputCls}
            value={form.tin}
            onChange={(e) => setForm({ ...form, tin: e.target.value })}
            required={isW9}
            placeholder={isW9 ? 'US taxpayer identification number' : 'Foreign TIN if any'}
          />
        </label>
        <label className={labelCls}>
          {isW9 ? 'TIN country (US required)' : 'TIN country'}
          <input
            className={inputCls}
            value={form.tin_country}
            onChange={(e) => setForm({ ...form, tin_country: e.target.value.toUpperCase() })}
            required={isW9}
            maxLength={2}
            placeholder={isW9 ? 'US' : 'ISO country code'}
          />
        </label>
        <label className={labelCls}>
          TIN kind (optional)
          <select
            className={inputCls}
            value={form.tin_kind}
            onChange={(e) => setForm({ ...form, tin_kind: e.target.value })}
          >
            <option value="">—</option>
            {api.SELF_CERT_TIN_KINDS.map((k) => (
              <option key={k} value={k}>
                {k}
              </option>
            ))}
          </select>
        </label>
        <div className="sm:col-span-2">
          <button type="submit" className={btnPrimary} disabled={submit.isPending}>
            {submit.isPending ? 'Submitting…' : 'Submit certification'}
          </button>
        </div>
      </form>
    </section>
  );
}
