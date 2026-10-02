/**
 * KYC API + constants — Task 10.3.24.
 *
 *   GET  /api/v1/kyc/status  → tier, lifecycle status, per-document states,
 *                              re-verification due date, trading-limit impact
 *   POST /api/v1/kyc/submit  → multi-step submission (Phase-12 Task 12.3.4;
 *                              documents stored S3+SSE-KMS server-side)
 *
 * Both routes are live (supersedes the earlier "Phase-12/14 stubs — UI
 * degrades on 501" note — the registry has zero StatusStub rows).
 *
 * Canonical values (spec §14.2 / Task 12.3.4):
 *   T0 → no trading, no withdrawals
 *   T1 → $10K/day withdrawal
 *   T2 → $100K/day withdrawal · re-verification every 12 months
 *   institutional → negotiated limits · re-verification every 24 months
 */
import type { ApiClient } from '@/lib/api';

export const KYC_TIERS = ['T0', 'T1', 'T2', 'INSTITUTIONAL'] as const;
export type KycTier = (typeof KYC_TIERS)[number];

/** §5.16 enum extended by remediation #35 — APPROVED is canonical,
 * EXPIRED drives the re-verification prompt. */
export const KYC_STATUSES = [
  'PENDING',
  'APPROVED',
  'REJECTED',
  'EXPIRED',
  'MANUAL_REVIEW',
] as const;
export type KycStatus = (typeof KYC_STATUSES)[number];

export const TIER_LIMITS: Record<
  KycTier,
  { withdrawal: string; trading: string; reverify: string }
> = {
  T0: { withdrawal: 'Withdrawals disabled', trading: 'Trading disabled', reverify: '—' },
  T1: { withdrawal: '$10,000 / day withdrawal', trading: 'Trading enabled', reverify: '—' },
  T2: {
    withdrawal: '$100,000 / day withdrawal',
    trading: 'Trading enabled',
    reverify: 'Re-verification every 12 months',
  },
  INSTITUTIONAL: {
    withdrawal: 'Negotiated limits',
    trading: 'Trading enabled',
    reverify: 'Re-verification every 24 months',
  },
};

export interface KycDocument {
  type: string; // GOVERNMENT_ID | PROOF_OF_ADDRESS | QUESTIONNAIRE | SELFIE
  status: string; // PENDING | APPROVED | REJECTED | EXPIRED
  submitted_at?: string;
  expires_at?: string;
  rejection_reason?: string;
}

export interface KycStatusResponse {
  tier: KycTier | (string & {});
  status: KycStatus | (string & {});
  documents: KycDocument[];
  re_verification_due?: string;
  submitted_at?: string;
  reviewed_at?: string;
  institutional?: boolean;
  message?: string;
}

export async function kycStatus(api: ApiClient): Promise<KycStatusResponse> {
  const res = await api.get<Partial<KycStatusResponse> | KycStatusResponse>('/kyc/status');
  return {
    tier: res.tier ?? 'T0',
    status: res.status ?? 'PENDING',
    documents: res.documents ?? [],
    re_verification_due: res.re_verification_due,
    submitted_at: res.submitted_at,
    reviewed_at: res.reviewed_at,
    institutional: res.institutional,
    message: res.message,
  };
}

export interface KycSubmission {
  personal: {
    first_name: string;
    last_name: string;
    date_of_birth: string;
    nationality: string;
    address: string;
  };
  documents: {
    type: string;
    filename: string;
    content_type: string;
    /** base64 file payload — ApiClient is JSON-only; the backend stores
     * to S3+SSE-KMS (Phase-12 Task 12.3.4). */
    data_base64: string;
  }[];
}

export async function submitKyc(api: ApiClient, sub: KycSubmission): Promise<void> {
  await api.post('/kyc/submit', sub);
}

export const DOC_TYPES = [
  { id: 'GOVERNMENT_ID', label: 'Government-issued ID (front/back)' },
  { id: 'PROOF_OF_ADDRESS', label: 'Proof of address (utility bill / bank statement)' },
  { id: 'QUESTIONNAIRE', label: 'Appropriateness questionnaire' },
  { id: 'SELFIE', label: 'Selfie / liveness photo' },
] as const;

/** Client-side upload rules (Task 10.3.24: type + ≤10 MB; the ≥200 DPI
 * check runs server-side — noted on the file input hint). */
export const MAX_DOC_BYTES = 10 * 1024 * 1024;
export const ALLOWED_DOC_TYPES = [
  'image/jpeg',
  'image/png',
  'image/webp',
  'application/pdf',
] as const;

export function validateDocFile(file: { name: string; size: number; type: string }): string | null {
  if (file.size > MAX_DOC_BYTES) return 'File exceeds the 10 MB limit';
  if (file.size === 0) return 'File is empty';
  if (!(ALLOWED_DOC_TYPES as readonly string[]).includes(file.type)) {
    return 'JPEG, PNG, WebP or PDF only';
  }
  return null;
}

export function fileToBase64(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => {
      const out = typeof r.result === 'string' ? r.result : '';
      const i = out.indexOf('base64,');
      resolve(i >= 0 ? out.slice(i + 7) : out);
    };
    r.onerror = () => {
      reject(new Error(`failed to read ${file.name}`));
    };
    r.readAsDataURL(file);
  });
}
