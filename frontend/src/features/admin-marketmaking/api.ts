/**
 * admin-marketmaking wire seam (Phase-10.5 Task 10.5.3.12) — RTS 6 /
 * DEA / market-maker program administration.
 *
 *   GET/POST/PUT    /api/v1/admin/mm-programs[?account_id=&status=]
 *   POST            …/mm-programs/{id}/{suspend,resume,mmp-reset}
 *   GET             …/mm-programs/{id}/{compliance,rebates}
 *   POST            /api/v1/admin/mm-programs/rebates/post   monthly GL sweep
 *   GET/POST        /api/v1/admin/algo-certifications[?status=]
 *   POST            …/algo-certifications/{id}/transition    CERTIFIED|SUSPENDED|REVOKED
 *   GET/POST        /api/v1/admin/dea/controls[?session_id=]
 *   POST            …/dea/controls/{session_id}/suspend
 *   GET/POST        /api/v1/admin/rts6/self-assessments
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
const bool = (v: unknown): boolean => v === true;
const arr = (v: unknown): unknown[] => (Array.isArray(v) ? v : []);
const envOpts = (env: AdminEnv) => ({ headers: { [ADMIN_ENV_HEADER]: env } });

// ---------------------------------------------------------------------------
// MM programs (Task 18.3.10).
// ---------------------------------------------------------------------------

export interface MMProgram {
  id: number;
  accountId: number;
  instrumentId?: number;
  symbol?: string;
  minQuoteSize: string;
  maxSpreadBps: string;
  presencePct: string;
  mmpMaxFills: number;
  mmpWindowMs: number;
  rebateBps: string;
  otrAllowance?: string;
  status: string;
}

function parseProgram(v: unknown): MMProgram | null {
  if (!isRecord(v)) return null;
  return {
    id: num(v['id']) ?? 0,
    accountId: num(v['account_id']) ?? 0,
    instrumentId: num(v['instrument_id']),
    symbol: str(v['symbol']),
    minQuoteSize: str(v['min_quote_size']) ?? '0',
    maxSpreadBps: str(v['max_spread_bps']) ?? '0',
    presencePct: str(v['presence_pct']) ?? '0',
    mmpMaxFills: num(v['mmp_max_fills']) ?? 0,
    mmpWindowMs: num(v['mmp_window_ms']) ?? 0,
    rebateBps: str(v['rebate_bps']) ?? '0',
    otrAllowance: str(v['otr_allowance']),
    status: str(v['status']) ?? '',
  };
}

export async function fetchMMPrograms(
  api: BoundAdminApi,
  f: { accountId?: number; status?: string },
): Promise<MMProgram[]> {
  const q = new URLSearchParams();
  if (f.accountId !== undefined && f.accountId > 0) q.set('account_id', String(f.accountId));
  if (f.status) q.set('status', f.status);
  const suffix = q.size > 0 ? `?${q.toString()}` : '';
  const raw = await api.get<unknown>(`/admin/mm-programs${suffix}`);
  if (!isRecord(raw)) throw malformed('mm programs');
  return arr(raw['mm_programs']).flatMap((p) => {
    const row = parseProgram(p);
    return row !== null ? [row] : [];
  });
}

export interface MMProgramInput {
  accountId: number;
  instrumentId?: number;
  minQuoteSize: string;
  maxSpreadBps: string;
  presencePct: string;
  mmpMaxFills: number;
  mmpWindowMs: number;
  rebateBps: string;
  otrAllowance?: string;
}

function programBody(i: MMProgramInput): Record<string, unknown> {
  return {
    account_id: i.accountId,
    instrument_id: i.instrumentId,
    min_quote_size: i.minQuoteSize,
    max_spread_bps: i.maxSpreadBps,
    presence_pct: i.presencePct,
    mmp_max_fills: i.mmpMaxFills,
    mmp_window_ms: i.mmpWindowMs,
    rebate_bps: i.rebateBps,
    otr_allowance: i.otrAllowance,
  };
}

export async function enrollMMProgram(
  api: BoundAdminApi,
  input: MMProgramInput,
): Promise<MMProgram> {
  const raw = await api.post<unknown>('/admin/mm-programs', programBody(input));
  const p = parseProgram(raw);
  if (p === null) throw malformed('mm program');
  return p;
}

export async function updateMMProgram(
  api: ApiClient,
  env: AdminEnv,
  id: number,
  input: MMProgramInput,
): Promise<MMProgram> {
  const raw = await api.put<unknown>(`/admin/mm-programs/${id}`, programBody(input), envOpts(env));
  const p = parseProgram(raw);
  if (p === null) throw malformed('mm program');
  return p;
}

export async function mmProgramAction(
  api: BoundAdminApi,
  id: number,
  verb: 'suspend' | 'resume' | 'mmp-reset',
  reason?: string,
): Promise<void> {
  await api.post(`/admin/mm-programs/${id}/${verb}`, reason !== undefined ? { reason } : {});
}

export interface MMComplianceRow {
  day: string;
  samplesTotal: number;
  samplesCompliant: number;
  presencePct?: string;
  breach: boolean;
  breachReason?: string;
}

export async function fetchMMCompliance(
  api: BoundAdminApi,
  id: number,
  f: { from?: string; to?: string },
): Promise<MMComplianceRow[]> {
  const q = new URLSearchParams();
  if (f.from) q.set('from', f.from);
  if (f.to) q.set('to', f.to);
  const suffix = q.size > 0 ? `?${q.toString()}` : '';
  const raw = await api.get<unknown>(`/admin/mm-programs/${id}/compliance${suffix}`);
  if (!isRecord(raw)) throw malformed('mm compliance');
  return arr(raw['compliance']).flatMap((c) => {
    if (!isRecord(c)) return [];
    return [
      {
        day: str(c['day']) ?? '',
        samplesTotal: num(c['samples_total']) ?? 0,
        samplesCompliant: num(c['samples_compliant']) ?? 0,
        presencePct: str(c['presence_pct']),
        breach: bool(c['breach']),
        breachReason: str(c['breach_reason']),
      },
    ];
  });
}

export interface MMRebate {
  id: number;
  day: string;
  currency: string;
  amount: string;
  postedJournalId?: number;
}

export async function fetchMMRebates(api: BoundAdminApi, id: number): Promise<MMRebate[]> {
  const raw = await api.get<unknown>(`/admin/mm-programs/${id}/rebates`);
  if (!isRecord(raw)) throw malformed('rebates');
  return arr(raw['rebates']).flatMap((r) => {
    if (!isRecord(r)) return [];
    return [
      {
        id: num(r['id']) ?? 0,
        day: str(r['day']) ?? '',
        currency: str(r['currency']) ?? '',
        amount: str(r['amount']) ?? '0',
        postedJournalId: num(r['posted_journal_id']),
      },
    ];
  });
}

/** Monthly GL sweep — may partially succeed; partial_error reported. */
export async function postMMRebates(
  api: BoundAdminApi,
): Promise<{ posted: number; partialError?: string }> {
  const raw = await api.post<unknown>('/admin/mm-programs/rebates/post', {});
  if (!isRecord(raw)) throw malformed('rebate sweep');
  return {
    posted: arr(raw['posted_journals']).length,
    partialError: str(raw['partial_error']),
  };
}

// ---------------------------------------------------------------------------
// Algo certifications (RTS 6, Task 21.3.12).
// ---------------------------------------------------------------------------

export interface AlgoCert {
  id: number;
  algoId: string;
  accountId: number;
  status: string;
  testEvidenceRef: string;
  killButtonTested: boolean;
  certifiedBy?: number;
  expiresAt?: string;
  reviewDueAt?: string;
}

export async function fetchAlgoCerts(api: BoundAdminApi, status?: string): Promise<AlgoCert[]> {
  const raw = await api.get<unknown>(
    `/admin/algo-certifications${status !== undefined && status !== '' ? `?status=${status}` : ''}`,
  );
  if (!isRecord(raw)) throw malformed('certifications');
  return arr(raw['certifications']).flatMap((c) => {
    if (!isRecord(c)) return [];
    return [
      {
        id: num(c['id']) ?? 0,
        algoId: str(c['algo_id']) ?? '',
        accountId: num(c['account_id']) ?? 0,
        status: str(c['status']) ?? '',
        testEvidenceRef: str(c['test_evidence_ref']) ?? '',
        killButtonTested: bool(c['kill_button_tested']),
        certifiedBy: num(c['certified_by']),
        expiresAt: str(c['expires_at']),
        reviewDueAt: str(c['review_due_at']),
      },
    ];
  });
}

export async function certifyAlgo(
  api: BoundAdminApi,
  input: {
    algoId: string;
    accountId: number;
    testEvidenceRef: string;
    killButtonTested: boolean;
    capacityAssessRef: string;
  },
): Promise<void> {
  await api.post('/admin/algo-certifications', {
    algo_id: input.algoId,
    account_id: input.accountId,
    test_evidence_ref: input.testEvidenceRef,
    kill_button_tested: input.killButtonTested,
    capacity_assessment_ref: input.capacityAssessRef,
  });
}

export async function transitionAlgoCert(
  api: BoundAdminApi,
  id: number,
  to: 'CERTIFIED' | 'SUSPENDED' | 'REVOKED',
  reason: string,
): Promise<void> {
  await api.post(`/admin/algo-certifications/${id}/transition`, { to, reason });
}

// ---------------------------------------------------------------------------
// DEA controls + RTS 6 self-assessments.
// ---------------------------------------------------------------------------

export interface DEAControl {
  id: number;
  sessionId: string;
  accountId: number;
  maxOrderQty: string;
  maxMsgsPerSec: number;
  sponsoringDesk: string;
  dropCopyFeed: string;
  status: string;
}

function parseDEA(v: unknown): DEAControl | null {
  if (!isRecord(v)) return null;
  return {
    id: num(v['id']) ?? 0,
    sessionId: str(v['session_id']) ?? '',
    accountId: num(v['account_id']) ?? 0,
    maxOrderQty: str(v['max_order_qty']) ?? '0',
    maxMsgsPerSec: num(v['max_msgs_per_sec']) ?? 0,
    sponsoringDesk: str(v['sponsoring_desk']) ?? '',
    dropCopyFeed: str(v['drop_copy_feed']) ?? '',
    status: str(v['status']) ?? '',
  };
}

export async function fetchDEAControl(
  api: BoundAdminApi,
  sessionId: string,
): Promise<DEAControl | null> {
  const raw = await api.get<unknown>(
    `/admin/dea/controls?session_id=${encodeURIComponent(sessionId)}`,
  );
  if (!isRecord(raw)) throw malformed('dea control');
  const c = raw['dea_control'];
  if (c === null || c === undefined) return null;
  const row = parseDEA(c);
  if (row === null) throw malformed('dea control');
  return row;
}

export async function setDEAControl(
  api: BoundAdminApi,
  input: {
    sessionId: string;
    accountId: number;
    maxOrderQty: string;
    maxMsgsPerSec: number;
    sponsoringDesk: string;
    dropCopyFeed: string;
  },
): Promise<void> {
  await api.post('/admin/dea/controls', {
    session_id: input.sessionId,
    account_id: input.accountId,
    max_order_qty: input.maxOrderQty,
    max_msgs_per_sec: input.maxMsgsPerSec,
    sponsoring_desk: input.sponsoringDesk,
    drop_copy_feed: input.dropCopyFeed,
  });
}

export async function suspendDEA(
  api: BoundAdminApi,
  sessionId: string,
  reason: string,
): Promise<void> {
  await api.post(`/admin/dea/controls/${encodeURIComponent(sessionId)}/suspend`, { reason });
}

export interface RTS6Assessment {
  id: number;
  periodYear: number;
  documentRef: string;
  status: string;
  filedBy: number;
  filedAt?: string;
}

export async function fetchRTS6Assessments(api: BoundAdminApi): Promise<RTS6Assessment[]> {
  const raw = await api.get<unknown>('/admin/rts6/self-assessments');
  if (!isRecord(raw)) throw malformed('assessments');
  return arr(raw['assessments']).flatMap((a) => {
    if (!isRecord(a)) return [];
    return [
      {
        id: num(a['id']) ?? 0,
        periodYear: num(a['period_year']) ?? 0,
        documentRef: str(a['document_ref']) ?? '',
        status: str(a['status']) ?? '',
        filedBy: num(a['filed_by']) ?? 0,
        filedAt: str(a['filed_at']),
      },
    ];
  });
}

export async function fileRTS6Assessment(
  api: BoundAdminApi,
  input: { periodYear: number; documentRef: string; dueAt: string },
): Promise<void> {
  await api.post('/admin/rts6/self-assessments', {
    period_year: input.periodYear,
    document_ref: input.documentRef,
    due_at: input.dueAt,
  });
}
