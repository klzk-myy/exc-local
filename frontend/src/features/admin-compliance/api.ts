/**
 * admin-compliance wire seam (Phase-10.5 Task 10.5.3.5) — day-to-day
 * client compliance ops: on-demand screening + adverse-media intake,
 * travel-rule MISSING_INFO cure, restricted-list administration,
 * employee-dealing pre-clearance + audit, and the enforcement ladder.
 *
 *   POST   /api/v1/admin/compliance/screening/accounts/{id}   → {outcome}
 *   POST   /api/v1/admin/compliance/screening/adverse-media   → 201
 *   GET    /api/v1/admin/sanctions/status                     → gate state
 *   GET    /api/v1/admin/travel-rule[?status=]                → {records}
 *   GET    /api/v1/admin/travel-rule/{id}                     → {record}
 *   POST   /api/v1/admin/travel-rule/{id}/supply              → {record}
 *   GET    /api/v1/admin/restricted-lists[?status=]           → {restricted_lists}
 *   POST   /api/v1/admin/restricted-lists
 *   DELETE /api/v1/admin/restricted-lists?id=N                → RETIRED
 *   GET    /api/v1/admin/pre-clearance[?outcome=]             → {pre_clearances}
 *   POST   /api/v1/admin/pre-clearance   (file | decide forms)
 *   GET    /api/v1/admin/employee-dealing/audit               → {audit}
 *   GET    /api/v1/admin/enforcement[?account_id=]            → {actions}
 *   POST   /api/v1/admin/enforcement/{signal_id}              → {action}
 *
 * BoundAdminApi exposes get/post/put/delete — each stamps
 * X-Admin-Env (Task 10.5.3.27).
 */
import { malformed } from '@/lib/admin/api';
import type { BoundAdminApi } from '@/lib/env';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined);
const num = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined;
const arr = (v: unknown): unknown[] => (Array.isArray(v) ? v : []);
const strList = (v: unknown): string[] => arr(v).filter((x): x is string => typeof x === 'string');
const numList = (v: unknown): number[] =>
  arr(v).filter((x): x is number => typeof x === 'number' && Number.isFinite(x));

// ---------------------------------------------------------------------------
// Screening (Task 21.3.1/21.3.11) + sanctions provider state.
// ---------------------------------------------------------------------------

export interface ScreenOutcome {
  accountId: number;
  pepHits: number;
  sanctionHits: number;
  quarantined: boolean;
  holdId?: string;
  adverseMediaCount: number;
  screenedAt: string;
}

export async function screenAccount(api: BoundAdminApi, accountId: number): Promise<ScreenOutcome> {
  const raw = await api.post<unknown>(`/admin/compliance/screening/accounts/${accountId}`, {});
  const o = isRecord(raw) && isRecord(raw['outcome']) ? raw['outcome'] : null;
  if (o === null || num(o['account_id']) === undefined) throw malformed('screen outcome');
  return {
    accountId: num(o['account_id']) ?? accountId,
    pepHits: arr(o['pep_hits']).length,
    sanctionHits: arr(o['sanction_hits']).length,
    quarantined: o['quarantined'] === true,
    holdId: str(o['hold_id']),
    adverseMediaCount: num(o['adverse_media_count']) ?? 0,
    screenedAt: str(o['screened_at']) ?? '',
  };
}

export async function reportAdverseMedia(
  api: BoundAdminApi,
  input: { accountId: number; headline: string; source?: string; url?: string; severity?: string },
): Promise<void> {
  await api.post('/admin/compliance/screening/adverse-media', {
    account_id: input.accountId,
    headline: input.headline,
    source: input.source,
    url: input.url,
    severity: input.severity,
  });
}

/** Provider-gate / queue / provenance snapshot — keys are opportunistic
 * server-side; render what arrives, never fabricate. */
export async function fetchSanctionsStatus(api: BoundAdminApi): Promise<Record<string, unknown>> {
  const raw = await api.get<unknown>('/admin/sanctions/status');
  if (!isRecord(raw)) throw malformed('sanctions status');
  return raw;
}

// ---------------------------------------------------------------------------
// Travel rule (Task 21.3.2) — MISSING_INFO cure queue.
// ---------------------------------------------------------------------------

export interface TravelRuleParty {
  name?: string;
  accountNumber?: string;
  address?: string;
  country?: string;
}

export interface TravelRuleRecord {
  id: number;
  transferId: number;
  direction: string;
  accountId: number;
  rail?: string;
  currency: string;
  amount?: string;
  status: string;
  missingFields: string[];
  originator: TravelRuleParty;
  beneficiary: TravelRuleParty;
  holdRef?: string;
  updatedAt: string;
}

function parseParty(v: unknown): TravelRuleParty {
  if (!isRecord(v)) return {};
  return {
    name: str(v['name']),
    accountNumber: str(v['account_number']),
    address: str(v['address']),
    country: str(v['country']),
  };
}

function parseTravelRecord(v: unknown): TravelRuleRecord | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  if (id === undefined) return null;
  return {
    id,
    transferId: num(v['transfer_id']) ?? 0,
    direction: str(v['direction']) ?? '',
    accountId: num(v['account_id']) ?? 0,
    rail: str(v['rail']),
    currency: str(v['currency']) ?? '',
    amount:
      v['amount'] !== undefined && v['amount'] !== null
        ? typeof v['amount'] === 'string'
          ? v['amount']
          : JSON.stringify(v['amount'])
        : undefined,
    status: str(v['status']) ?? '',
    missingFields: strList(v['missing_fields']),
    originator: parseParty(v['originator']),
    beneficiary: parseParty(v['beneficiary']),
    holdRef: str(v['hold_ref']),
    updatedAt: str(v['updated_at']) ?? '',
  };
}

export async function fetchTravelRule(
  api: BoundAdminApi,
  status?: string,
): Promise<TravelRuleRecord[]> {
  const raw = await api.get<unknown>('/admin/travel-rule', {
    status: status === '' ? undefined : status,
    limit: 100,
  });
  if (!isRecord(raw) || !Array.isArray(raw['records'])) throw malformed('travel-rule list');
  return (raw['records'] as unknown[])
    .map(parseTravelRecord)
    .filter((r): r is TravelRuleRecord => r !== null);
}

/** GET /admin/travel-rule/{id} — record detail (full party fields,
 * dispatch-hold provenance the list trims). */
export async function fetchTravelRuleRecord(
  api: BoundAdminApi,
  id: number,
): Promise<Record<string, unknown>> {
  const raw = await api.get<unknown>(`/admin/travel-rule/${id}`);
  if (!isRecord(raw)) throw malformed('travel-rule detail');
  return isRecord(raw['record']) ? raw['record'] : raw;
}

export async function supplyTravelRule(
  api: BoundAdminApi,
  id: number,
  supply: { originator?: TravelRuleParty; beneficiary?: TravelRuleParty },
): Promise<TravelRuleRecord> {
  const body: Record<string, unknown> = {};
  if (supply.originator !== undefined) {
    body['originator'] = {
      name: supply.originator.name,
      account_number: supply.originator.accountNumber,
      address: supply.originator.address,
      country: supply.originator.country,
    };
  }
  if (supply.beneficiary !== undefined) {
    body['beneficiary'] = {
      name: supply.beneficiary.name,
      account_number: supply.beneficiary.accountNumber,
      country: supply.beneficiary.country,
    };
  }
  const raw = await api.post<unknown>(`/admin/travel-rule/${id}/supply`, body);
  const rec = isRecord(raw) ? parseTravelRecord(raw['record']) : null;
  if (rec === null) throw malformed('travel-rule supply');
  return rec;
}

// ---------------------------------------------------------------------------
// Restricted lists (Task 21.3.24).
// ---------------------------------------------------------------------------

export interface RestrictedListRow {
  id: number;
  eventId: string;
  eventType: string;
  instruments: string[];
  windowStart: string;
  windowEnd: string;
  scope: string;
  scopeRole?: string;
  namedAccounts: number[];
  status: string;
  reason: string;
}

function parseRestrictedList(v: unknown): RestrictedListRow | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  if (id === undefined) return null;
  return {
    id,
    eventId: str(v['event_id']) ?? '',
    eventType: str(v['event_type']) ?? '',
    instruments: strList(v['instruments']),
    windowStart: str(v['window_start']) ?? '',
    windowEnd: str(v['window_end']) ?? '',
    scope: str(v['scope']) ?? '',
    scopeRole: str(v['scope_role']),
    namedAccounts: numList(v['named_accounts']),
    status: str(v['status']) ?? '',
    reason: str(v['reason']) ?? '',
  };
}

export async function fetchRestrictedLists(api: BoundAdminApi): Promise<RestrictedListRow[]> {
  const raw = await api.get<unknown>('/admin/restricted-lists', { limit: 100 });
  if (!isRecord(raw) || !Array.isArray(raw['restricted_lists']))
    throw malformed('restricted lists');
  return (raw['restricted_lists'] as unknown[])
    .map(parseRestrictedList)
    .filter((r): r is RestrictedListRow => r !== null);
}

export async function createRestrictedList(
  api: BoundAdminApi,
  input: {
    eventId: string;
    eventType: string;
    instruments: string[];
    windowStart: string;
    windowEnd: string;
    scope: string;
    scopeRole?: string;
    namedAccounts: number[];
    reason: string;
  },
): Promise<void> {
  await api.post('/admin/restricted-lists', {
    event_id: input.eventId,
    event_type: input.eventType,
    instruments: input.instruments,
    window_start: input.windowStart,
    window_end: input.windowEnd,
    widen_minutes: 0,
    scope: input.scope,
    scope_role: input.scopeRole ?? '',
    named_accounts: input.namedAccounts,
    reason: input.reason,
  });
}

export async function retireRestrictedList(api: BoundAdminApi, id: number): Promise<void> {
  await api.delete(`/admin/restricted-lists?id=${id}`);
}

// ---------------------------------------------------------------------------
// Employee dealing — pre-clearance queue + audit trail.
// ---------------------------------------------------------------------------

export interface PreClearance {
  id: number;
  requestId: string;
  accountId: number;
  instrument: string;
  side?: string;
  notionalCap?: string;
  reason: string;
  outcome: string;
  expiresAt: string;
  requestedBy: number;
  decisionNote?: string;
}

function parseClearance(v: unknown): PreClearance | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  if (id === undefined) return null;
  return {
    id,
    requestId: str(v['request_id']) ?? '',
    accountId: num(v['account_id']) ?? 0,
    instrument: str(v['instrument']) ?? '',
    side: str(v['side']),
    notionalCap: str(v['notional_cap']),
    reason: str(v['reason']) ?? '',
    outcome: str(v['outcome']) ?? '',
    expiresAt: str(v['expires_at']) ?? '',
    requestedBy: num(v['requested_by']) ?? 0,
    decisionNote: str(v['decision_note']),
  };
}

export async function fetchPreClearances(
  api: BoundAdminApi,
  outcome?: string,
): Promise<PreClearance[]> {
  const raw = await api.get<unknown>('/admin/pre-clearance', {
    outcome: outcome === '' || outcome === undefined ? undefined : outcome,
    limit: 100,
  });
  if (!isRecord(raw) || !Array.isArray(raw['pre_clearances'])) throw malformed('pre-clearances');
  return (raw['pre_clearances'] as unknown[])
    .map(parseClearance)
    .filter((p): p is PreClearance => p !== null);
}

/** Decision form — {id, approve, note}. */
export async function decidePreClearance(
  api: BoundAdminApi,
  id: number,
  approve: boolean,
  note: string,
): Promise<void> {
  await api.post('/admin/pre-clearance', { id, approve, note });
}

/** Officer-filed request form. */
export async function filePreClearance(
  api: BoundAdminApi,
  input: {
    accountId: number;
    instrument: string;
    side?: string;
    reason: string;
    expiresAt?: string;
  },
): Promise<void> {
  await api.post('/admin/pre-clearance', {
    account_id: input.accountId,
    instrument: input.instrument,
    side: input.side,
    reason: input.reason,
    expires_at: input.expiresAt ?? '',
  });
}

export interface DealingAuditRow {
  id: number;
  adminUserId: number;
  action: string;
  targetType: string;
  createdAt: string;
}

export async function fetchDealingAudit(api: BoundAdminApi): Promise<DealingAuditRow[]> {
  const raw = await api.get<unknown>('/admin/employee-dealing/audit', { limit: 50 });
  if (!isRecord(raw) || !Array.isArray(raw['audit'])) throw malformed('dealing audit');
  const out: DealingAuditRow[] = [];
  for (const v of raw['audit'] as unknown[]) {
    if (!isRecord(v) || num(v['id']) === undefined) continue;
    out.push({
      id: num(v['id']) ?? 0,
      adminUserId: num(v['admin_user_id']) ?? 0,
      action: str(v['action']) ?? '',
      targetType: str(v['target_type']) ?? '',
      createdAt: str(v['created_at']) ?? '',
    });
  }
  return out;
}

// ---------------------------------------------------------------------------
// Enforcement ladder (Task 21.3.8).
// ---------------------------------------------------------------------------

export const ENFORCE_ACTIONS = ['WARN', 'THROTTLE', 'RESTRICT', 'SUSPEND', 'DISMISS'] as const;

export interface EnforcementAction {
  actionId: string;
  signalId?: number;
  accountId?: number;
  action: string;
  source: string;
  status: string;
  actorId: number;
  note: string;
  expiresAt?: string;
  createdAt: string;
}

function parseEnforcement(v: unknown): EnforcementAction | null {
  if (!isRecord(v)) return null;
  const actionId = str(v['action_id']);
  const action = str(v['action']);
  if (actionId === undefined || action === undefined) return null;
  return {
    actionId,
    signalId: num(v['signal_id']),
    accountId: num(v['account_id']),
    action,
    source: str(v['source']) ?? '',
    status: str(v['status']) ?? '',
    actorId: num(v['actor_id']) ?? 0,
    note: str(v['note']) ?? '',
    expiresAt: str(v['expires_at']),
    createdAt: str(v['created_at']) ?? '',
  };
}

export async function fetchEnforcement(
  api: BoundAdminApi,
  accountId?: number,
): Promise<EnforcementAction[]> {
  const raw = await api.get<unknown>('/admin/enforcement', {
    account_id: accountId === undefined || accountId <= 0 ? undefined : accountId,
    limit: 100,
  });
  if (!isRecord(raw) || !Array.isArray(raw['actions'])) throw malformed('enforcement list');
  return (raw['actions'] as unknown[])
    .map(parseEnforcement)
    .filter((a): a is EnforcementAction => a !== null);
}

export async function enforce(
  api: BoundAdminApi,
  signalId: number,
  input: { action: string; note: string; ttlSeconds?: number },
): Promise<void> {
  await api.post(`/admin/enforcement/${signalId}`, {
    action: input.action,
    note: input.note,
    ttl_seconds: input.ttlSeconds ?? 0,
  });
}

// ---------------------------------------------------------------------------
// Swap-free account verification decisions (Phase-14 Task 14.3.15) —
// Compliance-Officer approve/reject on requests and revoke on the
// standing status (abuse → compliance hold). No admin list route
// mounts; ids arrive through the compliance case queue.
// ---------------------------------------------------------------------------

export const decideSwapFree = (
  api: BoundAdminApi,
  id: number,
  decision: 'approve' | 'reject' | 'revoke',
  reason?: string,
): Promise<unknown> => api.post<unknown>(`/admin/swap-free/${id}/${decision}`, { reason });
