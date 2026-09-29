/**
 * KYC upload wizard — Task 10.3.24 item 2. Four steps:
 *   1. personal info (name, DOB, nationality, address)
 *   2. government ID (front/back)
 *   3. proof of address
 *   4. questionnaire answers + optional selfie (institutional tier)
 * Files validate type + ≤10 MB client-side (DPI ≥200 enforced server-side),
 * then POST /kyc/submit as base64-in-JSON.
 */
import { useState } from 'react';
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

interface DocSlot {
  type: string;
  label: string;
  file: File | null;
  error: string | null;
}

const STEP_LABELS = [
  'Personal details',
  'Identity document',
  'Proof of address',
  'Review & submit',
];

function DocPicker({ slot, onFile }: { slot: DocSlot; onFile: (f: File | null) => void }) {
  return (
    <Field
      label={slot.label}
      required
      error={slot.error}
      hint="JPEG, PNG, WebP or PDF · max 10 MB · min 200 DPI (checked on review)"
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
}: {
  onSubmitted?: () => void;
  prefill?: Partial<Personal>;
}) {
  const [step, setStep] = useState(0);
  const [personal, setPersonal] = useState<Personal>({
    first_name: prefill?.first_name ?? '',
    last_name: prefill?.last_name ?? '',
    date_of_birth: prefill?.date_of_birth ?? '',
    nationality: prefill?.nationality ?? '',
    address: prefill?.address ?? '',
  });
  const [docs, setDocs] = useState<[DocSlot, DocSlot, DocSlot]>([
    {
      type: 'GOVERNMENT_ID',
      label: 'Government-issued ID (front or passport)',
      file: null,
      error: null,
    },
    { type: 'PROOF_OF_ADDRESS', label: 'Proof of address', file: null, error: null },
    { type: 'SELFIE', label: 'Selfie / liveness photo (optional)', file: null, error: null },
  ]);
  const [uploading, setUploading] = useState(false);

  const submit = useMutation({
    mutationFn: async () => {
      setUploading(true);
      const documents = [];
      for (const d of docs) {
        if (d.file === null) continue;
        documents.push({
          type: d.type,
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
    .filter((d) => d.type !== 'SELFIE')
    .every((d) => d.file !== null && d.error === null);

  // Tuple destructuring yields the fixed doc slots (state is always length-3).
  const [govIdSlot, poaSlot, selfieSlot] = docs;

  const setDoc = (i: number, f: File | null) => {
    setDocs((prev) => {
      const next = [...prev];
      const d = next[i];
      if (d === undefined) return prev;
      next[i] = { ...d, file: f, error: f === null ? null : api.validateDocFile(f) };
      return next as [DocSlot, DocSlot, DocSlot];
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
      <ol className="mb-4 flex gap-2" aria-label="Wizard progress">
        {STEP_LABELS.map((s, i) => (
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

      {step === 1 && (
        <DocPicker
          slot={govIdSlot}
          onFile={(f) => {
            setDoc(0, f);
          }}
        />
      )}
      {step === 2 && (
        <DocPicker
          slot={poaSlot}
          onFile={(f) => {
            setDoc(1, f);
          }}
        />
      )}

      {step === 3 && (
        <div>
          <DocPicker
            slot={selfieSlot}
            onFile={(f) => {
              setDoc(2, f);
            }}
          />
          <ul className="mb-4 list-inside list-disc text-sm text-neutral-400">
            {docs.map((d) => (
              <li key={d.type}>
                {d.label}:{' '}
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
        {step < 3 ? (
          <button
            type="button"
            className={btnPrimary}
            disabled={step === 0 && !personalValid}
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
