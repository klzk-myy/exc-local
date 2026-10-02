/**
 * Admin console wire seam — the five Task-8 review surfaces. Field
 * names mirror the Go JSON tags; nothing is fabricated client-side.
 *
 *   GET  /api/v1/admin/kyc/pending                    review queue (14.3.4)
 *   POST /api/v1/admin/kyc/{id}/approve | /reject     decision {reason}
 *   GET  /api/v1/admin/webhooks/dead-letters          DLQ (14.3.12)
 *   POST /api/v1/admin/webhooks/dead-letters/{id}/retransmit
 *   GET  /api/v1/admin/liquidity-providers            LP list (7.3.9)
 *   GET  /api/v1/admin/liquidity-providers/{id}/scorecard?window=
 *   GET  /api/v1/admin/bindings                       RBAC bindings (7.3.11)
 *   POST /api/v1/admin/bindings | /{id}/revoke        dual-control → 202
 *   GET  /api/v1/admin/compliance/holds               hold review (14.3.10)
 *   POST /api/v1/admin/compliance/holds/{id}/release  {approver_id, reason}
 *   POST /api/v1/admin/compliance/holds/{id}/escalate {disposition, reason}
 */
import { malformed } from '@/lib/admin/api';
import type { BoundAdminApi } from '@/lib/env';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined);
const num = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined;

// ---- KYC review queue ----

export interface KycQueueItem {
  id: number;
  accountId: number;
  requestedTier: string;
  status: string;
  jurisdiction: string;
  riskScore: number | null;
  submittedAt: string;
  slaDueAt: string;
}

export function parseSubmission(v: unknown): KycQueueItem | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  const accountId = num(v['account_id']);
  if (id === undefined || accountId === undefined) return null;
  return {
    id,
    accountId,
    requestedTier: str(v['requested_tier']) ?? '',
    status: str(v['status']) ?? '',
    jurisdiction: str(v['jurisdiction']) ?? '',
    riskScore: num(v['risk_score']) ?? null,
    submittedAt: str(v['submitted_at']) ?? '',
    slaDueAt: str(v['sla_due_at']) ?? '',
  };
}

export async function listKycPending(api: BoundAdminApi): Promise<KycQueueItem[]> {
  const res = await api.get<unknown>('/admin/kyc/pending');
  const raw = isRecord(res) && Array.isArray(res['submissions']) ? res['submissions'] : [];
  return raw.map(parseSubmission).filter((s): s is KycQueueItem => s !== null);
}

export async function decideKyc(api: BoundAdminApi, id: number, approve: boolean, reason = '') {
  if (approve) return api.post(`/admin/kyc/${id}/approve`, {});
  return api.post(`/admin/kyc/${id}/reject`, { reason });
}

// ---- Webhook DLQ ----

export interface DeadLetter {
  id: string;
  event: string;
  status: string;
  attempts: number;
  lastStatusCode: number | null;
  lastError: string | null;
}

export function parseDeadLetter(v: unknown): DeadLetter | null {
  if (!isRecord(v)) return null;
  const id =
    str(v['delivery_id']) ?? str(v['id']) ?? (num(v['id']) !== undefined ? String(v['id']) : null);
  if (!id) return null;
  return {
    id,
    event: str(v['event']) ?? '',
    status: str(v['status']) ?? '',
    attempts: num(v['attempts']) ?? 0,
    lastStatusCode: num(v['last_status_code']) ?? null,
    lastError: str(v['last_error']) ?? null,
  };
}

export async function listDeadLetters(api: BoundAdminApi): Promise<DeadLetter[]> {
  const res = await api.get<unknown>('/admin/webhooks/dead-letters', { limit: '200' });
  const raw = isRecord(res) && Array.isArray(res['dead_letters']) ? res['dead_letters'] : [];
  return raw.map(parseDeadLetter).filter((d): d is DeadLetter => d !== null);
}

export async function retransmitDeadLetter(api: BoundAdminApi, id: string) {
  return api.post(`/admin/webhooks/dead-letters/${id}/retransmit`, {});
}

// ---- LP scorecard ----

export interface LP {
  lpId: number;
  name: string;
  status: string;
  connectionType: string;
}

export interface LPScorecard {
  lpId: number;
  window: string;
  quotesReceived: number;
  fills: number;
  rejections: number;
  fillRatio: number | null;
  rejectionRate: number | null;
  avgResponseMs: number | null;
  p99LatencyMs: number | null;
  availabilityPct: number | null;
  stale: boolean;
}

export function parseLP(v: unknown): LP | null {
  if (!isRecord(v)) return null;
  const lpId = num(v['lp_id']) ?? num(v['id']);
  const name = str(v['name']);
  if (lpId === undefined || !name) return null;
  return {
    lpId,
    name,
    status: str(v['status']) ?? '',
    connectionType: str(v['connection_type']) ?? '',
  };
}

export function parseScorecard(v: unknown): LPScorecard | null {
  if (!isRecord(v)) return null;
  const lpId = num(v['lp_id']);
  if (lpId === undefined) return null;
  return {
    lpId,
    window: str(v['window']) ?? '',
    quotesReceived: num(v['quotes_received']) ?? 0,
    fills: num(v['fills']) ?? 0,
    rejections: num(v['rejections']) ?? 0,
    fillRatio: num(v['fill_ratio']) ?? null,
    rejectionRate: num(v['rejection_rate']) ?? null,
    avgResponseMs: num(v['avg_response_time_ms']) ?? null,
    p99LatencyMs: num(v['p99_latency_ms']) ?? null,
    availabilityPct: num(v['availability_pct']) ?? null,
    stale: v['stale'] === true,
  };
}

export async function listLPs(api: BoundAdminApi): Promise<LP[]> {
  const res = await api.get<unknown>('/admin/liquidity-providers');
  const raw =
    isRecord(res) && Array.isArray(res['liquidity_providers']) ? res['liquidity_providers'] : [];
  return raw.map(parseLP).filter((l): l is LP => l !== null);
}

export async function lpScorecard(api: BoundAdminApi, lpId: number, window = '1h') {
  const res = await api.get<unknown>(`/admin/liquidity-providers/${lpId}/scorecard`, { window });
  const sc = parseScorecard(res);
  if (!sc) throw malformed('LP scorecard');
  return sc;
}

// ---- RBAC bindings ----

export interface Binding {
  id: number;
  userId: number;
  role: string;
  status: string;
  expiresAt: string;
  grantedAt: string;
}

export function parseBinding(v: unknown): Binding | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  const userId = num(v['user_id']);
  const role = str(v['role']);
  if (id === undefined || userId === undefined || !role) return null;
  return {
    id,
    userId,
    role,
    status: str(v['status']) ?? '',
    expiresAt: str(v['expires_at']) ?? '',
    grantedAt: str(v['granted_at']) ?? '',
  };
}

export async function listBindings(api: BoundAdminApi): Promise<Binding[]> {
  const res = await api.get<unknown>('/admin/bindings');
  const raw = isRecord(res) && Array.isArray(res['bindings']) ? res['bindings'] : [];
  return raw.map(parseBinding).filter((b): b is Binding => b !== null);
}

export async function listRoles(api: BoundAdminApi): Promise<string[]> {
  const res = await api.get<unknown>('/admin/roles');
  const raw = isRecord(res) ? (res['roles'] ?? res['items']) : null;
  if (!Array.isArray(raw)) return [];
  return raw
    .map((r) => (typeof r === 'string' ? r : isRecord(r) ? str(r['name']) : undefined))
    .filter((r): r is string => r !== undefined);
}

/** Grant/revoke submit to the four-eyes queue — success is HTTP 202
 * {dual_control:"required", request}, never an immediate effect. */
export async function grantBinding(
  api: BoundAdminApi,
  input: { userId: number; role: string; expiresInSeconds: number; reason: string },
) {
  return api.post('/admin/bindings', {
    user_id: input.userId,
    role: input.role,
    expires_in_seconds: input.expiresInSeconds,
    reason: input.reason,
  });
}

export async function revokeBinding(api: BoundAdminApi, id: number, reason: string) {
  return api.post(`/admin/bindings/${id}/revoke`, { reason });
}

// ---- Compliance holds ----

export interface ComplianceHold {
  holdId: string;
  accountId: number;
  trigger: string;
  reason: string;
  status: string;
  slaDeadline: string;
  slaBreached: boolean;
  placedBy: number;
}

export function parseHold(v: unknown): ComplianceHold | null {
  if (!isRecord(v)) return null;
  const holdId = str(v['hold_id']);
  const accountId = num(v['account_id']);
  if (!holdId || accountId === undefined) return null;
  return {
    holdId,
    accountId,
    trigger: str(v['trigger_source']) ?? str(v['trigger']) ?? '',
    reason: str(v['reason']) ?? '',
    status: str(v['status']) ?? '',
    slaDeadline: str(v['sla_deadline']) ?? '',
    slaBreached: v['sla_breached'] === true,
    placedBy: num(v['placed_by']) ?? 0,
  };
}

export async function listHolds(api: BoundAdminApi, status = 'OPEN'): Promise<ComplianceHold[]> {
  const res = await api.get<unknown>('/admin/compliance/holds', { status, limit: '200' });
  const raw = isRecord(res) && Array.isArray(res['holds']) ? res['holds'] : [];
  return raw.map(parseHold).filter((h): h is ComplianceHold => h !== null);
}

export async function releaseHold(
  api: BoundAdminApi,
  holdId: string,
  approverId: number,
  reason: string,
) {
  return api.post(`/admin/compliance/holds/${holdId}/release`, {
    approver_id: approverId,
    reason,
  });
}

export async function escalateHold(
  api: BoundAdminApi,
  holdId: string,
  disposition: 'sar' | 'closure',
  reason: string,
) {
  return api.post(`/admin/compliance/holds/${holdId}/escalate`, { disposition, reason });
}
