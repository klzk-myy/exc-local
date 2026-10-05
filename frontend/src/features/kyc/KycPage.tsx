/**
 * /kyc — verification status tracker + document-upload wizard
 * (Task 10.3.24). Registration routes here on success; EXPIRED status
 * shows the re-verification prompt and the wizard for resubmission.
 */
import { useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { RequireAuth } from '@/features/auth/guards';
import { useAuthed } from '@/lib/trading/queries';
import { ErrorBox, cardCls } from '@/lib/ui';

import * as api from './api';
import { StatusTracker } from './StatusTracker';
import UploadWizard from './UploadWizard';

/** Tier policy block from GET /kyc/requirements — liveness/biometric
 * gates, rescreen cadence, re-verification interval, review SLA and the
 * tier's daily caps (null = negotiated/unlimited, rendered verbatim). */
function PolicyCard({ requirements }: { requirements: api.KycRequirements }) {
  const p = requirements.policy;
  const requiredCount = requirements.documents.filter((d) => d.required === true).length;
  const optionalCount = requirements.documents.length - requiredCount;
  if (p === null && requirements.documents.length === 0) {
    return (
      <div className={cardCls}>
        <h2 className="mb-2 text-sm font-semibold">Requirements for your tier</h2>
        <p className="text-sm text-neutral-400">
          No verification requirements are configured for your tier and jurisdiction.
        </p>
      </div>
    );
  }
  return (
    <div className={cardCls}>
      <h2 className="mb-2 text-sm font-semibold">
        Requirements for your tier{p !== null && p.tier !== '' ? ` — ${p.tier}` : ''}
      </h2>
      {p !== null && (
        <>
          {p.description !== undefined && p.description !== '' && (
            <p className="mb-2 text-sm text-neutral-400">{p.description}</p>
          )}
          <dl className="grid grid-cols-2 gap-x-4 gap-y-1 text-sm md:grid-cols-3">
            <div>
              <dt className="text-neutral-500">Liveness check</dt>
              <dd>{p.liveness_required === true ? 'Required' : 'Not required'}</dd>
            </div>
            <div>
              <dt className="text-neutral-500">Biometric verification</dt>
              <dd>{p.biometric_required === true ? 'Required' : 'Not required'}</dd>
            </div>
            <div>
              <dt className="text-neutral-500">Rescreen cadence</dt>
              <dd>{p.rescreen_cadence ?? '—'}</dd>
            </div>
            <div>
              <dt className="text-neutral-500">Re-verification</dt>
              <dd>
                {p.reverify_months !== undefined && p.reverify_months > 0
                  ? `Every ${p.reverify_months} months`
                  : 'Not periodic'}
              </dd>
            </div>
            <div>
              <dt className="text-neutral-500">Manual review SLA</dt>
              <dd>
                {p.manual_review_sla_hours !== undefined ? `${p.manual_review_sla_hours}h` : '—'}
              </dd>
            </div>
            <div>
              <dt className="text-neutral-500">Daily withdrawal cap</dt>
              <dd>{p.daily_withdrawal_usd ?? 'Negotiated'}</dd>
            </div>
            <div>
              <dt className="text-neutral-500">Daily trading cap</dt>
              <dd>{p.daily_trading_usd ?? 'Unlimited'}</dd>
            </div>
          </dl>
        </>
      )}
      <p className="mt-3 text-xs text-neutral-500">
        {requiredCount} required document{requiredCount === 1 ? '' : 's'}
        {optionalCount > 0 ? ` · ${optionalCount} optional` : ''} — see the checklist below.
      </p>
    </div>
  );
}

export default function KycPage() {
  const qc = useQueryClient();
  const authed = useAuthed();
  const q = useQuery({
    queryKey: ['kyc', 'status'],
    queryFn: () => api.kycStatus(apiClient),
    retry: false,
    enabled: authed,
  });
  // Task 10.5.3.24 — merged ops-matrix (tier+jurisdiction resolved
  // server-side from the session) drives the wizard's document checklist.
  const req = useQuery({
    queryKey: ['kyc', 'requirements'],
    queryFn: () => api.kycRequirements(apiClient),
    retry: false,
    enabled: authed,
  });

  const status = q.data;
  const needsSubmission =
    status === undefined ||
    status.status === 'EXPIRED' ||
    status.status === 'REJECTED' ||
    status.tier === 'T0';

  return (
    <RequireAuth>
      <div className="mx-auto max-w-3xl p-6">
        <h1 className="mb-4 text-xl font-semibold">Identity verification (KYC)</h1>
        <div className="space-y-4">
          {q.isError && (
            <div className={cardCls}>
              <ErrorBox error={q.error} />
              <p className="text-sm text-neutral-400">
                The verification service is unavailable — you can still prepare your documents
                below.
              </p>
            </div>
          )}
          {q.isPending && <p className="text-sm text-neutral-400">Loading status…</p>}
          {status !== undefined && <StatusTracker status={status} />}
          {req.data !== undefined && <PolicyCard requirements={req.data} />}
          {needsSubmission && (
            <UploadWizard
              requirements={req.data}
              requirementsFailed={req.isError}
              onSubmitted={() => {
                void qc.invalidateQueries({ queryKey: ['kyc', 'status'] });
              }}
            />
          )}
        </div>
      </div>
    </RequireAuth>
  );
}
