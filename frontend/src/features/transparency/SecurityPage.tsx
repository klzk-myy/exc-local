/**
 * Public security page (Task 10.5.3.23) — no auth required.
 *
 *   GET  /security/policy       the VDP policy document (text/markdown)
 *   POST /security/disclosures  researcher intake → 201 {report_id,
 *                               status, duplicate}; the `website` field
 *                               is a server-side honeypot (hidden).
 */
import { useState } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError } from '@/lib/api';
import {
  ErrorBox,
  btnPrimary,
  cardCls,
  inputCls,
  labelCls,
  selectCls,
  textareaCls,
} from '@/lib/ui';

import { fetchSecurityPolicy, submitDisclosure } from './api';

const SEVERITIES = ['LOW', 'MEDIUM', 'HIGH', 'CRITICAL'];

function PolicyCard() {
  const q = useQuery({
    queryKey: ['public', 'security-policy'],
    queryFn: fetchSecurityPolicy,
    retry: false,
  });
  return (
    <section aria-label="Security policy" className={cardCls}>
      <h2 className="mb-1 text-sm font-semibold">Vulnerability disclosure policy</h2>
      {q.isPending ? (
        <p className="text-xs text-neutral-500">Loading policy…</p>
      ) : q.isError ? (
        <ErrorBox error={q.error} />
      ) : (
        <pre className="max-h-96 overflow-y-auto whitespace-pre-wrap rounded border border-neutral-800 bg-neutral-950 p-3 text-xs text-neutral-300">
          {q.data}
        </pre>
      )}
    </section>
  );
}

function DisclosureForm() {
  const [title, setTitle] = useState('');
  const [components, setComponents] = useState('');
  const [repro, setRepro] = useState('');
  const [handle, setHandle] = useState('');
  const [email, setEmail] = useState('');
  const [severity, setSeverity] = useState('MEDIUM');
  const [attr, setAttr] = useState(false);
  const [website, setWebsite] = useState(''); // honeypot — must stay empty
  const [result, setResult] = useState<{
    ok: boolean;
    reportId?: string;
    status?: string;
    duplicate?: boolean;
  } | null>(null);

  const submit = useMutation({
    mutationFn: () =>
      submitDisclosure(
        {
          title,
          affectedComponents: components
            .split(',')
            .map((s) => s.trim())
            .filter((s) => s !== ''),
          reproduction: repro,
          reporterHandle: handle,
          contactEmail: email,
          suggestedSeverity: severity,
          attributionRequested: attr,
        },
        apiClient,
      ),
    onSuccess: (r) => {
      setResult({ ok: true, reportId: r.reportId, status: r.status, duplicate: r.duplicate });
    },
  });

  return (
    <section aria-label="Report a vulnerability" className={cardCls}>
      <h2 className="mb-1 text-sm font-semibold">Report a vulnerability</h2>
      <p className="mb-3 text-xs text-neutral-500">
        Submissions enter the triage queue with a tracking id. Leave the website field empty — it is
        a spam honeypot.
      </p>
      <form
        className="space-y-2"
        onSubmit={(e) => {
          e.preventDefault();
          setResult(null);
          submit.mutate();
        }}
      >
        <div className="grid grid-cols-1 gap-2 md:grid-cols-2">
          <div>
            <label className={labelCls} htmlFor="vdp-title">
              Title
            </label>
            <input
              id="vdp-title"
              className={inputCls}
              value={title}
              onChange={(e) => {
                setTitle(e.target.value);
              }}
              required
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="vdp-sev">
              Suggested severity
            </label>
            <select
              id="vdp-sev"
              className={selectCls}
              value={severity}
              onChange={(e) => {
                setSeverity(e.target.value);
              }}
            >
              {SEVERITIES.map((s) => (
                <option key={s}>{s}</option>
              ))}
            </select>
          </div>
          <div>
            <label className={labelCls} htmlFor="vdp-handle">
              Reporter handle
            </label>
            <input
              id="vdp-handle"
              className={inputCls}
              value={handle}
              onChange={(e) => {
                setHandle(e.target.value);
              }}
              required
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="vdp-email">
              Contact email
            </label>
            <input
              id="vdp-email"
              type="email"
              className={inputCls}
              value={email}
              onChange={(e) => {
                setEmail(e.target.value);
              }}
            />
          </div>
        </div>
        <div>
          <label className={labelCls} htmlFor="vdp-components">
            Affected components (comma-separated)
          </label>
          <input
            id="vdp-components"
            className={inputCls}
            value={components}
            onChange={(e) => {
              setComponents(e.target.value);
            }}
            placeholder="api-gateway, order-ticket"
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="vdp-repro">
            Reproduction steps
          </label>
          <textarea
            id="vdp-repro"
            className={textareaCls}
            value={repro}
            onChange={(e) => {
              setRepro(e.target.value);
            }}
            required
          />
        </div>
        {/* Honeypot — hidden from users; the backend accept-and-drops any
            submission that fills it, so do not mark it aria-hidden. */}
        <div className="absolute -left-[9999px]" aria-hidden="true">
          <label htmlFor="vdp-website">Website</label>
          <input
            id="vdp-website"
            tabIndex={-1}
            autoComplete="off"
            value={website}
            onChange={(e) => {
              setWebsite(e.target.value);
            }}
          />
        </div>
        <label className="flex items-center gap-2 text-xs text-neutral-400">
          <input
            type="checkbox"
            checked={attr}
            onChange={(e) => {
              setAttr(e.target.checked);
            }}
          />
          Request public attribution when the report is resolved
        </label>
        <div className="flex items-center gap-3">
          <button type="submit" className={btnPrimary} disabled={submit.isPending}>
            {submit.isPending ? 'Submitting…' : 'Submit report'}
          </button>
          {result?.ok === true ? (
            <p role="status" className="text-xs text-emerald-400">
              Received{result.reportId !== undefined ? ` — report ${result.reportId}` : ''}
              {result.duplicate === true ? ' (duplicate of an existing report)' : ''}
            </p>
          ) : null}
        </div>
        {submit.isError ? <ErrorBox error={submit.error} /> : null}
        {submit.isError && submit.error instanceof ApiError && submit.error.status === 429 ? (
          <p className="text-xs text-amber-400">Rate limited — try again later.</p>
        ) : null}
      </form>
    </section>
  );
}

export default function SecurityPage() {
  return (
    <div className="mx-auto max-w-4xl space-y-4 p-4">
      <header>
        <h1 className="text-xl font-semibold text-neutral-100">Security</h1>
        <p className="text-sm text-neutral-500">
          Vulnerability disclosure policy and researcher intake.
        </p>
      </header>
      <PolicyCard />
      <DisclosureForm />
    </div>
  );
}
