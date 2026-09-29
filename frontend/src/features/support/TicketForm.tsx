/**
 * Ticket submission form — POST /api/v1/support/tickets.
 *   - category select; COMPLAINT shows statutory/MiFID wording
 *   - attachments ≤5 MB each (base64 forward-compat field)
 *   - URGENT priority gated: T2+/institutional, or a financial-impact
 *     justification (checked client-side; the venue re-checks)
 */
import { useState } from 'react';
import { useMutation } from '@tanstack/react-query';
import { useNavigate } from 'react-router';

import { apiClient } from '@/app/runtime';
import { useSessionStore } from '@/lib/auth/session';
import { fileToBase64 } from '@/features/kyc/api';
import { ErrorBox, Field, btnPrimary, cardCls, inputCls, selectCls, textareaCls } from '@/lib/ui';

import * as api from './api';

const CATEGORY_LABELS: Record<api.TicketCategory, string> = {
  FUNDING: 'Funding / payments',
  TRADING: 'Trading',
  KYC: 'Verification (KYC)',
  TECHNICAL: 'Technical issue',
  COMPLAINT: 'Formal complaint',
};

export default function TicketForm() {
  const navigate = useNavigate();
  const kycTier = useSessionStore((s) => s.user?.kycTier ?? null);
  const [category, setCategory] = useState<api.TicketCategory>('TECHNICAL');
  const [subject, setSubject] = useState('');
  const [body, setBody] = useState('');
  const [priority, setPriority] = useState<(typeof api.TICKET_PRIORITIES)[number]>('NORMAL');
  const [justification, setJustification] = useState('');
  const [files, setFiles] = useState<File[]>([]);
  const [fileError, setFileError] = useState<string | null>(null);

  const urgentOk = priority !== 'URGENT' || api.urgentAllowed(kycTier, justification);

  const create = useMutation({
    mutationFn: async () => {
      const attachments = await Promise.all(
        files.map(async (f) => ({
          filename: f.name,
          content_type: f.type || 'application/octet-stream',
          data_base64: await fileToBase64(f),
        })),
      );
      return api.createTicket(apiClient, {
        category,
        subject,
        body,
        priority,
        urgencyJustification: justification === '' ? undefined : justification,
        attachments: attachments.length > 0 ? attachments : undefined,
      });
    },
    onSuccess: (t) => {
      void navigate(`/support/tickets/${t.ticket_id}`);
    },
  });

  return (
    <div className={cardCls}>
      <h2 className="mb-3 text-sm font-semibold">New support ticket</h2>
      {category === 'COMPLAINT' && (
        <div className="mb-3 rounded border border-amber-700/50 bg-amber-950/30 p-3 text-xs text-amber-200">
          <p className="mb-1 font-medium">Formal complaint (MiFID complaint handling)</p>
          <p>
            Complaints are acknowledged within <strong>8 business hours</strong> and receive a final
            response within <strong>8 weeks</strong>. If you remain dissatisfied, you may be
            entitled to escalate to your national competent authority or an approved ADR scheme —
            details will accompany the final response.
          </p>
        </div>
      )}
      <ErrorBox error={create.error} />
      <form
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <Field label="Category" required>
          {(id, describedBy, invalid) => (
            <select
              id={id}
              aria-describedby={describedBy}
              aria-invalid={invalid}
              className={selectCls}
              value={category}
              onChange={(e) => {
                setCategory(e.target.value as api.TicketCategory);
              }}
            >
              {api.TICKET_CATEGORIES.map((c) => (
                <option key={c} value={c}>
                  {CATEGORY_LABELS[c]}
                </option>
              ))}
            </select>
          )}
        </Field>
        <Field label="Subject" required>
          {(id, describedBy, invalid) => (
            <input
              id={id}
              aria-describedby={describedBy}
              aria-invalid={invalid}
              className={inputCls}
              value={subject}
              onChange={(e) => {
                setSubject(e.target.value);
              }}
            />
          )}
        </Field>
        <Field label="Describe the issue" required>
          {(id, describedBy, invalid) => (
            <textarea
              id={id}
              aria-describedby={describedBy}
              aria-invalid={invalid}
              className={textareaCls}
              value={body}
              onChange={(e) => {
                setBody(e.target.value);
              }}
            />
          )}
        </Field>
        <Field label="Priority">
          {(id, describedBy, invalid) => (
            <select
              id={id}
              aria-describedby={describedBy}
              aria-invalid={invalid}
              className={selectCls}
              value={priority}
              onChange={(e) => {
                setPriority(e.target.value as (typeof api.TICKET_PRIORITIES)[number]);
              }}
            >
              {api.TICKET_PRIORITIES.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </select>
          )}
        </Field>
        {priority === 'URGENT' && (
          <Field
            label="Financial-impact justification"
            required
            error={
              !urgentOk
                ? 'Required — urgent priority needs T2 verification or a justification'
                : null
            }
            hint="Urgent tickets are for time-critical financial impact (e.g. stuck withdrawal)."
          >
            {(id, describedBy, invalid) => (
              <input
                id={id}
                aria-describedby={describedBy}
                aria-invalid={invalid || !urgentOk}
                className={inputCls}
                value={justification}
                onChange={(e) => {
                  setJustification(e.target.value);
                }}
              />
            )}
          </Field>
        )}
        <Field label="Attachments" error={fileError} hint="Screenshots or logs · max 5 MB each">
          {(id, describedBy, invalid) => (
            <input
              id={id}
              aria-describedby={describedBy}
              aria-invalid={invalid || fileError !== null}
              className={inputCls}
              type="file"
              multiple
              onChange={(e) => {
                const picked = Array.from(e.target.files ?? []);
                const bad = picked.find((f) => f.size > api.MAX_ATTACHMENT_BYTES);
                if (bad !== undefined) {
                  setFileError(`${bad.name} exceeds the 5 MB limit`);
                  return;
                }
                setFileError(null);
                setFiles(picked);
              }}
            />
          )}
        </Field>
        {files.length > 0 && (
          <ul className="mb-3 text-xs text-neutral-400">
            {files.map((f) => (
              <li key={f.name}>
                {f.name} ({Math.round(f.size / 1024)} KB)
              </li>
            ))}
          </ul>
        )}
        <button
          type="submit"
          className={btnPrimary}
          disabled={create.isPending || subject === '' || body === '' || !urgentOk}
        >
          {create.isPending ? 'Submitting…' : 'Submit ticket'}
        </button>
        <p className="mt-2 text-xs text-neutral-500">
          We acknowledge new tickets within {api.ACK_SLA_BUSINESS_HOURS} business hours.
        </p>
      </form>
    </div>
  );
}
