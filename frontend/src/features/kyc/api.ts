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

// ---------------------------------------------------------------------------
// Requirements matrix — GET /api/v1/kyc/requirements (Phase-12 Task
// 12.3.13). Called without params: the server resolves the caller's
// tier + jurisdiction from the session and returns the merged ops-matrix
// rows ('*' defaults + exact-jurisdiction overlays).
// ---------------------------------------------------------------------------

export interface KycTierPolicy {
  tier: string;
  description?: string;
  liveness_required?: boolean;
  biometric_required?: boolean;
  rescreen_cadence?: string; // NONE | WEEKLY | DAILY
  reverify_months?: number; // 0 = no periodic re-verification
  manual_review_sla_hours?: number;
  step_up_score?: number;
  decline_score?: number;
  /** Marshalled decimal; null = negotiated/unlimited. */
  daily_withdrawal_usd?: string | null;
  daily_trading_usd?: string | null;
}

export interface KycRequirementRow {
  id?: number;
  tier?: string;
  jurisdiction?: string;
  vendor?: string;
  document_type: string;
  doc_group?: string;
  required?: boolean;
  max_doc_age_days?: number | null;
  doc_expiry_lead_days?: number | null;
  notes?: string;
}

export interface KycRequirements {
  policy: KycTierPolicy | null;
  documents: KycRequirementRow[];
}

export async function kycRequirements(api: ApiClient): Promise<KycRequirements> {
  const res = await api.get<Partial<KycRequirements>>('/kyc/requirements');
  return { policy: res.policy ?? null, documents: res.documents ?? [] };
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

// ---------------------------------------------------------------------------
// Self-certification (Task 10.5.3.27 gate-coverage wiring) — investor
// categorisation + appropriateness declaration. GET returns the stored
// declaration; POST submits {annual_income, net_worth, trading_experience,
// acknowledges_risk}.
// ---------------------------------------------------------------------------

export interface SelfCertification {
  annualIncome?: string;
  netWorth?: string;
  tradingExperience?: string;
  acknowledgesRisk?: boolean;
  submittedAt?: string;
}

export async function fetchSelfCert(api: ApiClient): Promise<SelfCertification | null> {
  const raw = await api.get<unknown>('/kyc/self-certification');
  if (typeof raw !== 'object' || raw === null) return null;
  const r = raw as Record<string, unknown>;
  if (Object.keys(r).length === 0) return null;
  return {
    annualIncome: typeof r['annual_income'] === 'string' ? r['annual_income'] : undefined,
    netWorth: typeof r['net_worth'] === 'string' ? r['net_worth'] : undefined,
    tradingExperience:
      typeof r['trading_experience'] === 'string' ? r['trading_experience'] : undefined,
    acknowledgesRisk: r['acknowledges_risk'] === true,
    submittedAt: typeof r['submitted_at'] === 'string' ? r['submitted_at'] : undefined,
  };
}

export const postSelfCert = (
  api: ApiClient,
  body: {
    annual_income: string;
    net_worth: string;
    trading_experience: string;
    acknowledges_risk: boolean;
  },
): Promise<unknown> => api.post<unknown>('/kyc/self-certification', body);
