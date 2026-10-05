/**
 * KYC upload wizard — Task 10.3.24 item 2, extended by Task 10.5.3.24.
 * Step 1 is personal info (name, DOB, nationality, address); the document
 * steps are driven by GET /kyc/requirements (merged ops-matrix rows —
 * required rows become mandatory slots, optional rows attachable); the
 * last step is review & submit. When the requirements query is
 * unavailable the static Task-10.3.24 grid is used with a disclosure.
 * Files validate type + ≤10 MB client-side (DPI ≥200 enforced server-side),
 * then POST /kyc/submit as base64-in-JSON.
 */
import { useEffect, useMemo, useState } from 'react';
import { useMutation } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ErrorBox, Field, btnGhost, btnPrimary, cardCls, inputCls } from '@/lib/ui';

import * as api from './api';

interface Personal {
  first_name: string;
  last_name: string;
  date_of_birth: string;
  nationality: string;
  address: string;
}

interface DocSlotSpec {
  type: string;
  label: string;
  required: boolean;
  hint: string | null;
}

interface DocSlot {
  spec: DocSlotSpec;
  file: File | null;
  error: string | null;
}

function docLabel(t: string): string {
  return api.DOC_TYPES.find((d) => d.id === t)?.label ?? t.replaceAll('_', ' ');
}

/** Build the document checklist from the ops-matrix rows
 * (GET /kyc/requirements). Rows are merged by document_type — any row
 * marked required makes the slot mandatory. Falls back to the static
 * Task-10.3.24 grid only when the matrix is unavailable. */
function buildDocSlots(req: api.KycRequirements | null | undefined): DocSlotSpec[] {
  if (req == null) {
    return [
      {
        type: 'GOVERNMENT_ID',
        label: 'Government-issued ID (front or passport)',
        required: true,
        hint: null,
      },
      { type: 'PROOF_OF_ADDRESS', label: 'Proof of address', required: true, hint: null },
      { type: 'SELFIE', label: 'Selfie / liveness photo (optional)', required: false, hint: null },
    ];
  }
  const map = new Map<string, DocSlotSpec>();
  for (const row of req.documents) {
    const t = row.document_type;
    const cur = map.get(t);
    const hint =
      [
        row.doc_group !== undefined && row.doc_group !== '' ? `Group ${row.doc_group}` : null,
        row.max_doc_age_days !== null && row.max_doc_age_days !== undefined
          ? `issued within ${row.max_doc_age_days} days`
          : null,
        row.doc_expiry_lead_days !== null && row.doc_expiry_lead_days !== undefined
          ? `valid ≥${row.doc_expiry_lead_days} days beyond submission`
          : null,
        row.notes ?? null,
      ]
        .filter((x) => x !== null)
        .join(' · ') || null;
    if (cur === undefined) {
      map.set(t, { type: t, label: docLabel(t), required: row.required === true, hint });
    } else {
      cur.required = cur.required || row.required === true;
      cur.hint ??= hint;
    }
  }
  return [...map.values()].sort((a, b) => Number(b.required) - Number(a.required));
}

function DocPicker({ slot, onFile }: { slot: DocSlot; onFile: (f: File | null) => void }) {
  const baseHint = 'JPEG, PNG, WebP or PDF · max 10 MB · min 200 DPI (checked on review)';
  return (
    <Field
      label={slot.spec.required ? slot.spec.label : `${slot.spec.label} (optional)`}
      required={slot.spec.required}
      error={slot.error}
      hint={slot.spec.hint !== null ? `${baseHint} · ${slot.spec.hint}` : baseHint}
    >
      {(id, describedBy, invalid) => (
        <input
          id={id}
          aria-describedby={describedBy}
          aria-invalid={invalid || slot.error !== null}
          className={inputCls}
          type="file"
          accept={api.ALLOWED_DOC_TYPES.join(',')}
          onChange={(e) => {
            onFile(e.target.files?.[0] ?? null);
          }}
        />
      )}
    </Field>
  );
}

export default function UploadWizard({
  onSubmitted,
  prefill,
  requirements,
  requirementsFailed,
}: {
  onSubmitted?: () => void;
  prefill?: Partial<Personal>;
  /** Merged ops-matrix from GET /kyc/requirements — undefined while the
   * query is in flight; the static grid is used when it fails. */
  requirements?: api.KycRequirements;
  /** True when the requirements query failed — renders the static grid
   * plus an explicit "defaults shown" disclosure. */
  requirementsFailed?: boolean;
}) {
  const [step, setStep] = useState(0);
  const [personal, setPersonal] = useState<Personal>({
    first_name: prefill?.first_name ?? '',
    last_name: prefill?.last_name ?? '',
    date_of_birth: prefill?.date_of_birth ?? '',
    nationality: prefill?.nationality ?? '',
    address: prefill?.address ?? '',
  });
  const requirementsPending = requirements === undefined && requirementsFailed !== true;
  const slotSpecs = useMemo(() => buildDocSlots(requirements ?? null), [requirements]);
  const [docs, setDocs] = useState<DocSlot[]>(() =>
    slotSpecs.map((spec) => ({ spec, file: null, error: null })),
  );
  // Rebuild slots when the matrix resolves (or a different account loads);
  // already-attached files carry over by document_type.
  useEffect(() => {
    setDocs((prev) =>
      slotSpecs.map((spec) => {
        const prior = prev.find((d) => d.spec.type === spec.type);
        return { spec, file: prior?.file ?? null, error: prior?.error ?? null };
      }),
    );
  }, [slotSpecs]);
  const [uploading, setUploading] = useState(false);

  const submit = useMutation({
    mutationFn: async () => {
      setUploading(true);
      const documents = [];
      for (const d of docs) {
        if (d.file === null) continue;
        documents.push({
          type: d.spec.type,
          filename: d.file.name,
          content_type: d.file.type,
          data_base64: await api.fileToBase64(d.file),
        });
      }
      await api.submitKyc(apiClient, { personal, documents });
    },
    onSettled: () => {
      setUploading(false);
    },
    onSuccess: () => {
      onSubmitted?.();
    },
  });

  const personalValid =
    personal.first_name !== '' &&
    personal.last_name !== '' &&
    personal.date_of_birth !== '' &&
    personal.nationality !== '' &&
    personal.address !== '';

  const requiredDocsPresent = docs
    .filter((d) => d.spec.required)
    .every((d) => d.file !== null && d.error === null);

  const stepLabels = ['Personal details', ...docs.map((d) => d.spec.label), 'Review & submit'];
  const reviewStep = stepLabels.length - 1;
  const docSlot = step > 0 && step < reviewStep ? docs[step - 1] : undefined;

  const setDoc = (i: number, f: File | null) => {
    setDocs((prev) => {
      const next = [...prev];
      const d = next[i];
      if (d === undefined) return prev;
      next[i] = { ...d, file: f, error: f === null ? null : api.validateDocFile(f) };
      return next;
    });
  };

  const textField = (
    label: string,
    key: keyof Personal,
    opts: { type?: string; autoComplete?: string } = {},
  ) => (
    <Field label={label} required>
      {(id, describedBy, invalid) => (
        <input
          id={id}
          aria-describedby={describedBy}
          aria-invalid={invalid}
          className={inputCls}
          type={opts.type ?? 'text'}
          autoComplete={opts.autoComplete}
          value={personal[key]}
          onChange={(e) => {
            setPersonal((p) => ({ ...p, [key]: e.target.value }));
          }}
        />
      )}
    </Field>
  );

  return (
    <div className={cardCls}>
      <ol className="mb-4 flex flex-wrap gap-2" aria-label="Wizard progress">
        {stepLabels.map((s, i) => (
          <li
            key={s}
            className={`rounded px-2 py-1 text-xs ${
              i === step
                ? 'bg-sky-600 text-white'
                : i < step
                  ? 'bg-neutral-800 text-neutral-300'
                  : 'bg-neutral-900 text-neutral-500'
            }`}
            aria-current={i === step ? 'step' : undefined}
          >
            {i + 1}. {s}
          </li>
        ))}
      </ol>

      <ErrorBox error={submit.error} />

      {step === 0 && (
        <div>
          {textField('First name', 'first_name', { autoComplete: 'given-name' })}
          {textField('Last name', 'last_name', { autoComplete: 'family-name' })}
          {textField('Date of birth', 'date_of_birth', { type: 'date', autoComplete: 'bday' })}
          {textField('Nationality (ISO country)', 'nationality')}
          {textField('Residential address', 'address', { autoComplete: 'street-address' })}
        </div>
      )}

      {step > 0 && step < reviewStep && (
        <div>
          {requirementsPending ? (
            <p className="text-sm text-neutral-400" role="status">
              Loading document requirements…
            </p>
          ) : (
            <>
              {requirementsFailed === true && step === 1 && (
                <p className="mb-3 text-xs text-amber-400" role="status">
                  The requirements service is unavailable — showing the default document checklist.
                </p>
              )}
              {docSlot !== undefined && (
                <DocPicker
                  slot={docSlot}
                  onFile={(f) => {
                    setDoc(step - 1, f);
                  }}
                />
              )}
              {docs.length === 0 && (
                <p className="text-sm text-neutral-400">
                  No documents are required for your tier and jurisdiction.
                </p>
              )}
            </>
          )}
        </div>
      )}

      {step === reviewStep && (
        <div>
          <ul className="mb-4 list-inside list-disc text-sm text-neutral-400">
            {docs.map((d) => (
              <li key={d.spec.type}>
                {d.spec.label}
                {d.spec.required ? '' : ' (optional)'}:{' '}
                {d.file === null
                  ? 'not attached'
                  : `${d.file.name} (${Math.round(d.file.size / 1024)} KB)`}
              </li>
            ))}
          </ul>
          {uploading && (
            <p className="mb-2 text-sm text-neutral-300" role="status">
              Uploading documents…
            </p>
          )}
          {submit.isSuccess && (
            <p className="mb-2 text-sm text-emerald-400" role="status">
              Submitted — your documents are in review.
            </p>
          )}
        </div>
      )}

      <div className="flex justify-between">
        <button
          type="button"
          className={btnGhost}
          disabled={step === 0}
          onClick={() => {
            setStep((s) => s - 1);
          }}
        >
          Back
        </button>
        {step < reviewStep ? (
          <button
            type="button"
            className={btnPrimary}
            disabled={
              (step === 0 && !personalValid) ||
              (step > 0 && (requirementsPending || docs.length === 0))
            }
            onClick={() => {
              setStep((s) => s + 1);
            }}
          >
            Next
          </button>
        ) : (
          <button
            type="button"
            className={btnPrimary}
            disabled={!requiredDocsPresent || submit.isPending}
            onClick={() => {
              submit.mutate();
            }}
          >
            {submit.isPending ? 'Submitting…' : 'Submit for verification'}
          </button>
        )}
      </div>
    </div>
  );
}
