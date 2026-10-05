/**
 * admin-finance wire seam (Phase-10.5 Task 10.5.3.10) — pricing and
 * house-finance administration.
 *
 *   GET/POST        /api/v1/admin/funding/fees          schedule list/create v1
 *   GET/PUT/DELETE  /api/v1/admin/funding/fees/{id}       detail/successor/retire
 *   GET             /api/v1/admin/funding/fees/{id}/versions
 *   GET/POST        /api/v1/admin/fees/promo[s]           promo windows
 *   POST            /api/v1/admin/fees/promo/{id}/{approve|reject}  (approve = DC)
 *   GET             /api/v1/admin/finance/trial-balance   ?date=&currency=
 *   GET             /api/v1/admin/finance/{pnl,balance-sheet}  ?format=csv|parquet
 *   GET             /api/v1/admin/invoices[?account=&month=]
 *   GET/POST        /api/v1/admin/reporting-values
 *   GET/POST        /api/v1/admin/tax-reporting/runs
 *   POST            …/runs/{id}/{review,approve,reject,submit}   (approve = DC)
 *   GET             …/runs/{id}/xml                       golden artifact
 */
import { malformed } from '@/lib/admin/api';
import type { ApiClient } from '@/lib/api/client';
import { ADMIN_ENV_HEADER, type AdminEnv, type BoundAdminApi } from '@/lib/env';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined);
const num = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined;
const arr = (v: unknown): unknown[] => (Array.isArray(v) ? v : []);
const envOpts = (env: AdminEnv) => ({ headers: { [ADMIN_ENV_HEADER]: env } });

// ---------------------------------------------------------------------------
// Funding fee schedules (Task 11.3.9) — versioned; PUT inserts a
// successor, DELETE retires (the row stays on the audit record).
// ---------------------------------------------------------------------------

export interface FundingFeeTier {
  id: number;
  rail: string;
  currency: string;
  direction: string;
  accountTier: string;
  flatFee: string;
  percentageBps: string;
  minFee: string;
  maxFee?: string;
  freeTierMonthlyCount: number;
  effectiveDate: string;
  version: number;
  supersedesId?: number;
  retiredAt?: string;
}

function parseFeeTier(v: unknown): FundingFeeTier {
  if (!isRecord(v)) throw malformed('fee tier');
  return {
    id: num(v['id']) ?? 0,
    rail: str(v['rail']) ?? '',
    currency: str(v['currency']) ?? '',
    direction: str(v['direction']) ?? '',
    accountTier: str(v['account_tier']) ?? '',
    flatFee: str(v['flat_fee']) ?? '0',
    percentageBps: str(v['percentage_bps']) ?? '0',
    minFee: str(v['min_fee']) ?? '0',
    maxFee: str(v['max_fee']),
    freeTierMonthlyCount: num(v['free_tier_monthly_count']) ?? 0,
    effectiveDate: str(v['effective_date']) ?? '',
    version: num(v['version']) ?? 0,
    supersedesId: num(v['supersedes_id']),
    retiredAt: str(v['retired_at']),
  };
}

export async function fetchFundingFees(
  api: BoundAdminApi,
  f: { rail?: string; currency?: string; direction?: string; all?: boolean },
): Promise<FundingFeeTier[]> {
  const q = new URLSearchParams();
  if (f.rail) q.set('rail', f.rail);
  if (f.currency) q.set('currency', f.currency);
  if (f.direction) q.set('direction', f.direction);
  if (f.all === true) q.set('all', '1');
  const suffix = q.size > 0 ? `?${q.toString()}` : '';
  const raw = await api.get<unknown>(`/admin/funding/fees${suffix}`);
  if (!isRecord(raw)) throw malformed('fee list');
  return arr(raw['items']).flatMap((t) => (isRecord(t) ? [parseFeeTier(t)] : []));
}

export interface FeeTierInput {
  flatFee: string;
  percentageBps: string;
  minFee: string;
  maxFee?: string;
}

export async function createFundingFee(
  api: BoundAdminApi,
  input: FeeTierInput & {
    rail: string;
    currency: string;
    direction: string;
    accountTier: string;
  },
): Promise<FundingFeeTier> {
  const raw = await api.post<unknown>('/admin/funding/fees', {
    rail: input.rail,
    currency: input.currency,
    direction: input.direction,
    account_tier: input.accountTier,
    flat_fee: input.flatFee,
    percentage_bps: input.percentageBps,
    min_fee: input.minFee,
    max_fee: input.maxFee ?? null,
  });
  return parseFeeTier(raw);
}

export async function updateFundingFee(
  api: ApiClient,
  env: AdminEnv,
  id: number,
  input: FeeTierInput,
): Promise<FundingFeeTier> {
  const raw = await api.put<unknown>(
    `/admin/funding/fees/${id}`,
    {
      flat_fee: input.flatFee,
      percentage_bps: input.percentageBps,
      min_fee: input.minFee,
      max_fee: input.maxFee ?? null,
    },
    envOpts(env),
  );
  return parseFeeTier(raw);
}

export async function retireFundingFee(api: ApiClient, env: AdminEnv, id: number): Promise<void> {
  await api.delete(`/admin/funding/fees/${id}`, envOpts(env));
}

/** GET /admin/funding/fees/{id} — single schedule row detail (full row:
 * effective date, provenance, audit trail anchors the list trims). */
export async function fetchFundingFee(
  api: BoundAdminApi,
  id: number,
): Promise<Record<string, unknown>> {
  const raw = await api.get<unknown>(`/admin/funding/fees/${id}`);
  if (!isRecord(raw)) throw malformed('fee detail');
  return raw;
}

export async function fetchFeeVersions(api: BoundAdminApi, id: number): Promise<FundingFeeTier[]> {
  const raw = await api.get<unknown>(`/admin/funding/fees/${id}/versions`);
  if (!isRecord(raw)) throw malformed('fee versions');
  return arr(raw['items']).flatMap((t) => (isRecord(t) ? [parseFeeTier(t)] : []));
}

// ---------------------------------------------------------------------------
// Fee promos (Task 5.3.15) — create opens PENDING_APPROVAL; approve is
// dual-control (distinct approver resolved from claims server-side).
// ---------------------------------------------------------------------------

export interface PromoWindow {
  id: number;
  feeTierId: number;
  promoMakerBps?: string;
  promoTakerBps?: string;
  endsAt: string;
  status: string;
  createdBy: number;
  approvedBy?: number;
}

export async function fetchPromoWindows(api: BoundAdminApi): Promise<PromoWindow[]> {
  const raw = await api.get<unknown>('/admin/fees/promos');
  if (!isRecord(raw)) throw malformed('promo list');
  return arr(raw['promo_windows']).flatMap((w) => {
    if (!isRecord(w)) return [];
    return [
      {
        id: num(w['id']) ?? 0,
        feeTierId: num(w['fee_tier_id']) ?? 0,
        promoMakerBps: str(w['promo_maker_bps']),
        promoTakerBps: str(w['promo_taker_bps']),
        endsAt: str(w['ends_at']) ?? '',
        status: str(w['status']) ?? '',
        createdBy: num(w['created_by']) ?? 0,
        approvedBy: num(w['approved_by']),
      },
    ];
  });
}

export async function createPromoWindow(
  api: BoundAdminApi,
  input: {
    feeTierId: number;
    promoMakerBps?: string;
    promoTakerBps?: string;
    endsAt: string;
    note?: string;
  },
): Promise<void> {
  await api.post('/admin/fees/promo', {
    fee_tier_id: input.feeTierId,
    promo_maker_bps: input.promoMakerBps,
    promo_taker_bps: input.promoTakerBps,
    ends_at: input.endsAt,
    note: input.note ?? '',
  });
}

export async function approvePromo(api: BoundAdminApi, id: number): Promise<void> {
  await api.post(`/admin/fees/promo/${id}/approve`, {});
}

export async function rejectPromo(api: BoundAdminApi, id: number, reason: string): Promise<void> {
  await api.post(`/admin/fees/promo/${id}/reject`, { reason });
}

// ---------------------------------------------------------------------------
// Finance statements + invoices + reporting values.
// ---------------------------------------------------------------------------

export interface TrialBalanceCurrency {
  currency: string;
  totalDebits: string;
  totalCredits: string;
  assets: string;
  liabilities: string;
  equity: string;
  revenue: string;
  expenses: string;
  netIncome: string;
}

export interface TrialBalance {
  businessDate: string;
  generatedAt: string;
  currencies: TrialBalanceCurrency[];
}

export async function fetchTrialBalance(
  api: BoundAdminApi,
  f: { date?: string; currency?: string },
): Promise<TrialBalance> {
  const q = new URLSearchParams();
  if (f.date) q.set('date', f.date);
  if (f.currency) q.set('currency', f.currency);
  const suffix = q.size > 0 ? `?${q.toString()}` : '';
  const raw = await api.get<unknown>(`/admin/finance/trial-balance${suffix}`);
  if (!isRecord(raw)) throw malformed('trial balance');
  return {
    businessDate: str(raw['business_date']) ?? '',
    generatedAt: str(raw['generated_at']) ?? '',
    currencies: arr(raw['currencies']).flatMap((c) => {
      if (!isRecord(c)) return [];
      return [
        {
          currency: str(c['currency']) ?? '',
          totalDebits: str(c['total_debits']) ?? '0',
          totalCredits: str(c['total_credits']) ?? '0',
          assets: str(c['assets']) ?? '0',
          liabilities: str(c['liabilities']) ?? '0',
          equity: str(c['equity']) ?? '0',
          revenue: str(c['revenue']) ?? '0',
          expenses: str(c['expenses']) ?? '0',
          netIncome: str(c['net_income']) ?? '0',
        },
      ];
    }),
  };
}

export interface Invoice {
  invoiceId: number;
  accountId: number;
  month: string;
  currency: string;
  tradingFees: string;
  mmRebates: string;
  connectivityFees: string;
  total: string;
  status: string;
  contentSha256?: string;
}

export async function fetchInvoices(
  api: BoundAdminApi,
  f: { account?: number; month?: string },
): Promise<Invoice[]> {
  const q = new URLSearchParams();
  if (f.account !== undefined && f.account > 0) q.set('account', String(f.account));
  if (f.month) q.set('month', f.month);
  const suffix = q.size > 0 ? `?${q.toString()}` : '';
  const raw = await api.get<unknown>(`/admin/invoices${suffix}`);
  if (!isRecord(raw)) throw malformed('invoices');
  return arr(raw['invoices']).flatMap((i) => {
    if (!isRecord(i)) return [];
    return [
      {
        invoiceId: num(i['invoice_id']) ?? 0,
        accountId: num(i['account_id']) ?? 0,
        month: str(i['month']) ?? '',
        currency: str(i['currency']) ?? '',
        tradingFees: str(i['trading_fees']) ?? '0',
        mmRebates: str(i['mm_rebates']) ?? '0',
        connectivityFees: str(i['connectivity_fees']) ?? '0',
        total: str(i['total']) ?? '0',
        status: str(i['status']) ?? '',
        contentSha256: str(i['content_sha256']),
      },
    ];
  });
}

export interface ReportingValue {
  id: number;
  scope: string;
  key: string;
  value: unknown;
  source: string;
  effectiveFrom: string;
}

export async function fetchReportingValues(
  api: BoundAdminApi,
  scope?: string,
): Promise<ReportingValue[]> {
  const raw = await api.get<unknown>(
    `/admin/reporting-values${scope !== undefined && scope !== '' ? `?scope=${encodeURIComponent(scope)}` : ''}`,
  );
  if (!isRecord(raw)) throw malformed('reporting values');
  return arr(raw['reporting_values']).flatMap((v) => {
    if (!isRecord(v)) return [];
    return [
      {
        id: num(v['id']) ?? 0,
        scope: str(v['scope']) ?? '',
        key: str(v['key']) ?? '',
        value: v['value'],
        source: str(v['source']) ?? '',
        effectiveFrom: str(v['effective_from']) ?? '',
      },
    ];
  });
}

export async function setReportingValue(
  api: BoundAdminApi,
  input: { scope: string; key: string; value: string; source: string },
): Promise<void> {
  let parsed: unknown = input.value;
  try {
    parsed = JSON.parse(input.value);
  } catch {
    parsed = input.value; // scalar string is a valid reporting value
  }
  await api.post('/admin/reporting-values', {
    scope: input.scope,
    key: input.key,
    value: parsed,
    source: input.source,
  });
}

// ---------------------------------------------------------------------------
// Tax reporting (Task 21.3.22) — DRAFT→UNDER_REVIEW→APPROVED→SUBMITTED,
// approve is dual-control (approver ≠ creator/reviewer, claims-side).
// ---------------------------------------------------------------------------

export interface TaxRun {
  id: number;
  regime: string;
  reportYear: number;
  jurisdiction: string;
  version: number;
  status: string;
  artifactRef?: string;
  sha256?: string;
  accountCount: number;
  createdBy: number;
  reviewedBy?: number;
  approvedBy?: number;
  submissionRef?: string;
  rejectionReason?: string;
}

function parseTaxRun(v: unknown): TaxRun {
  if (!isRecord(v)) throw malformed('tax run');
  return {
    id: num(v['id']) ?? 0,
    regime: str(v['regime']) ?? '',
    reportYear: num(v['report_year']) ?? 0,
    jurisdiction: str(v['jurisdiction']) ?? '',
    version: num(v['version']) ?? 0,
    status: str(v['status']) ?? '',
    artifactRef: str(v['artifact_ref']),
    sha256: str(v['sha256']),
    accountCount: num(v['account_count']) ?? 0,
    createdBy: num(v['created_by']) ?? 0,
    reviewedBy: num(v['reviewed_by']),
    approvedBy: num(v['approved_by']),
    submissionRef: str(v['submission_ref']),
    rejectionReason: str(v['rejection_reason']),
  };
}

export async function fetchTaxRuns(api: BoundAdminApi, regime?: string): Promise<TaxRun[]> {
  const raw = await api.get<unknown>(
    `/admin/tax-reporting/runs${regime !== undefined && regime !== '' ? `?regime=${regime}` : ''}`,
  );
  if (!isRecord(raw)) throw malformed('tax runs');
  return arr(raw['runs']).flatMap((r) => (isRecord(r) ? [parseTaxRun(r)] : []));
}

export async function generateTaxRun(
  api: BoundAdminApi,
  input: { regime: string; reportYear: number; jurisdiction: string },
): Promise<TaxRun> {
  const raw = await api.post<unknown>('/admin/tax-reporting/runs', {
    regime: input.regime,
    report_year: input.reportYear,
    jurisdiction: input.jurisdiction,
  });
  if (!isRecord(raw)) throw malformed('tax run');
  return parseTaxRun(raw['run'] ?? raw);
}

/** GET /admin/tax-reporting/runs/{id} — run detail (submission refs,
 * approval lineage, artifact hashes the list view trims). */
export async function fetchTaxRun(
  api: BoundAdminApi,
  id: number,
): Promise<Record<string, unknown>> {
  const raw = await api.get<unknown>(`/admin/tax-reporting/runs/${id}`);
  if (!isRecord(raw)) throw malformed('tax run detail');
  return isRecord(raw['run']) ? raw['run'] : raw;
}

export async function taxRunTransition(
  api: BoundAdminApi,
  id: number,
  verb: 'review' | 'approve' | 'reject' | 'submit',
  body?: Record<string, unknown>,
): Promise<TaxRun> {
  const raw = await api.post<unknown>(`/admin/tax-reporting/runs/${id}/${verb}`, body ?? {});
  if (!isRecord(raw)) throw malformed('tax run');
  return parseTaxRun(raw['run'] ?? raw);
}
