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

export default function KycPage() {
  const qc = useQueryClient();
  const authed = useAuthed();
  const q = useQuery({
    queryKey: ['kyc', 'status'],
    queryFn: () => api.kycStatus(apiClient),
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
          {needsSubmission && (
            <UploadWizard
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
