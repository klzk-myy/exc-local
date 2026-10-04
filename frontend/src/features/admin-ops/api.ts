/**
 * Ops-safety console wire seam (Phase-10.5 Task 10.5.3.1) — the
 * fail-safe levers: kill switch, circuit breaker, feature flags,
 * IP bans/allowlist, maintenance windows, and the destructive
 * trade-ops endpoints. All routes are already mounted; this module
 * only binds them.
 *
 *   GET/POST  /api/v1/admin/kill-switch            + POST .../reset
 *   POST      /api/v1/admin/circuit-breaker/{symbol}[.../reset]
 *   GET/POST/PUT/POST/DELETE /api/v1/admin/flags[/{name}[/advance]]
 *   GET/PUT/DELETE /api/v1/admin/ip-bans[/{ip}] + GET .../audit
 *   PUT/DELETE /api/v1/admin/ip-allowlist/{ip}
 *   GET/POST/PATCH/DELETE /api/v1/admin/maintenance-windows[/{id}]
 *   POST      /api/v1/admin/orders/mass-cancel
 *   POST      /api/v1/admin/liquidation/manual
 *   POST      /api/v1/admin/cache/warm
 *   PUT       /api/v1/admin/fix-sessions/{id}
 *
 * BoundAdminApi exposes get/post only — PUT/PATCH/DELETE go through
 * the raw ApiClient with the X-Admin-Env stamp applied by hand (same
 * contract as features/admin/api.ts).
 */
import type { ApiClient } from '@/lib/api/client';
import { malformed } from '@/lib/admin/api';
import { ADMIN_ENV_HEADER, type AdminEnv, type BoundAdminApi } from '@/lib/env';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined);
const num = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined;
const bool = (v: unknown): boolean | undefined => (typeof v === 'boolean' ? v : undefined);

const envOpts = (env: AdminEnv) => ({ headers: { [ADMIN_ENV_HEADER]: env } });

// ---------------------------------------------------------------------------
// Kill switch (Phase-11 Tasks 11.3.4/11.3.8/11.3.12 backend).
// ---------------------------------------------------------------------------

export const KILL_SWITCH_SCOPES = [
  'GLOBAL',
  'ACCOUNT',
  'COUNTERPARTY',
  'INSTRUMENT',
  'INSTRUMENT_CLASS',
  'FIX_SESSION',
  'LP',
  'RAIL',
  'REGION',
  'ENV',
  'DESK',
] as const;
export type KillSwitchScope = (typeof KILL_SWITCH_SCOPES)[number];

export interface Suspension {
  suspensionId: number;
  scope: string;
  targetId: string;
  reason: string;
  state: string;
  initiatedBy: number;
  approvedBy?: number;
  clearedBy?: number;
  clearApprovedBy?: number;
  clearedReason?: string;
  clientIp?: string;
  createdAt?: string;
  clearedAt?: string;
}

function parseSuspension(v: unknown): Suspension | null {
  if (!isRecord(v)) return null;
  const suspensionId = num(v['suspension_id']);
  const scope = str(v['scope']);
  if (suspensionId === undefined || scope === undefined) return null;
  return {
    suspensionId,
    scope,
    targetId: str(v['target_id']) ?? '',
    reason: str(v['reason']) ?? '',
    state: str(v['state']) ?? '',
    initiatedBy: num(v['initiated_by']) ?? 0,
    approvedBy: num(v['approved_by']),
    clearedBy: num(v['cleared_by']),
    clearApprovedBy: num(v['clear_approved_by']),
    clearedReason: str(v['cleared_reason']),
    clientIp: str(v['client_ip']),
    createdAt: str(v['created_at']),
    clearedAt: str(v['cleared_at']),
  };
}

export async function fetchSuspensions(api: BoundAdminApi): Promise<Suspension[]> {
  const raw = await api.get<unknown>('/admin/kill-switch');
  if (!isRecord(raw) || !Array.isArray(raw['suspensions'])) throw malformed('suspension list');
  return (raw['suspensions'] as unknown[])
    .map(parseSuspension)
    .filter((s): s is Suspension => s !== null);
}

export interface KillSwitchInput {
  scope: KillSwitchScope;
  targetId: string;
  reason: string;
  approverId: number;
}

/** POST /admin/kill-switch — trip. Returns the recorded suspension. */
export async function tripKillSwitch(
  api: BoundAdminApi,
  input: KillSwitchInput,
): Promise<Suspension> {
  const raw = await api.post<unknown>('/admin/kill-switch', {
    scope: input.scope,
    target_id: input.targetId,
    reason: input.reason,
    approver_id: input.approverId,
  });
  const s = parseSuspension(raw);
  if (s === null) throw malformed('kill-switch response');
  return s;
}

/** POST /admin/kill-switch/reset — clear an active suspension. */
export async function resetKillSwitch(
  api: BoundAdminApi,
  input: KillSwitchInput,
): Promise<Suspension> {
  const raw = await api.post<unknown>('/admin/kill-switch/reset', {
    scope: input.scope,
    target_id: input.targetId,
    reason: input.reason,
    approver_id: input.approverId,
  });
  const s = parseSuspension(raw);
  if (s === null) throw malformed('kill-switch reset response');
  return s;
}

// ---------------------------------------------------------------------------
// Circuit breaker (Phase-13 backend). Path param is the symbol; the body
// may override scope/target for ACCOUNT/MARKET_WIDE tiers.
// ---------------------------------------------------------------------------

export const CB_SCOPES = ['INSTRUMENT', 'ACCOUNT', 'MARKET_WIDE'] as const;
export type CircuitBreakerScope = (typeof CB_SCOPES)[number];

export interface CircuitBreakerInput {
  symbol: string;
  scope: CircuitBreakerScope;
  targetId: string;
  reason: string;
}

export interface CircuitBreakerTripResult {
  scope: string;
  targetId: string;
  state: string;
}

export async function tripCircuitBreaker(
  api: BoundAdminApi,
  input: CircuitBreakerInput,
): Promise<CircuitBreakerTripResult> {
  const raw = await api.post<unknown>(
    `/admin/circuit-breaker/${encodeURIComponent(input.symbol)}`,
    { scope: input.scope, target_id: input.targetId, reason: input.reason },
  );
  if (!isRecord(raw)) throw malformed('circuit-breaker trip');
  return {
    scope: str(raw['scope']) ?? input.scope,
    targetId: str(raw['target_id']) ?? input.targetId,
    state: str(raw['state']) ?? 'OPEN',
  };
}

/** Reset submits a dual-control request — 202 {dual_control, request}. */
export interface DualControlSubmit {
  dualControl: string;
  requestId?: number;
}

export async function resetCircuitBreaker(
  api: BoundAdminApi,
  input: CircuitBreakerInput,
): Promise<DualControlSubmit> {
  const raw = await api.post<unknown>(
    `/admin/circuit-breaker/${encodeURIComponent(input.symbol)}/reset`,
    { scope: input.scope, target_id: input.targetId, reason: input.reason },
  );
  if (!isRecord(raw)) throw malformed('circuit-breaker reset');
  const req = isRecord(raw['request']) ? raw['request'] : {};
  return {
    dualControl: str(raw['dual_control']) ?? 'required',
    requestId: num(req['id']) ?? num(req['request_id']),
  };
}

// ---------------------------------------------------------------------------
// Feature flags (Phase-09 Task 9.3.7 backend).
// ---------------------------------------------------------------------------

export interface FeatureFlag {
  name: string;
  enabled: boolean;
  rolloutPct?: number;
  stages?: number[];
  stageIdx?: number;
  tiers?: string[];
  accounts?: number[];
  description?: string;
}

function parseFlag(v: unknown): FeatureFlag | null {
  if (!isRecord(v)) return null;
  const name = str(v['name']);
  if (name === undefined) return null;
  return {
    name,
    enabled: bool(v['enabled']) ?? false,
    rolloutPct: num(v['rollout_pct']),
    stages: Array.isArray(v['stages'])
      ? (v['stages'] as unknown[]).filter((s): s is number => typeof s === 'number')
      : undefined,
    stageIdx: num(v['stage_idx']),
    tiers: Array.isArray(v['tiers'])
      ? (v['tiers'] as unknown[]).filter((s): s is string => typeof s === 'string')
      : undefined,
    accounts: Array.isArray(v['accounts'])
      ? (v['accounts'] as unknown[]).filter((s): s is number => typeof s === 'number')
      : undefined,
    description: str(v['description']),
  };
}

export async function fetchFlags(api: BoundAdminApi): Promise<FeatureFlag[]> {
  const raw = await api.get<unknown>('/admin/flags');
  if (!isRecord(raw) || !Array.isArray(raw['flags'])) throw malformed('flag list');
  return (raw['flags'] as unknown[]).map(parseFlag).filter((f): f is FeatureFlag => f !== null);
}

export async function createFlag(
  api: BoundAdminApi,
  input: { name: string; enabled: boolean; description?: string },
): Promise<FeatureFlag> {
  const raw = await api.post<unknown>('/admin/flags', {
    name: input.name,
    enabled: input.enabled,
    description: input.description,
  });
  const f = parseFlag(raw);
  if (f === null) throw malformed('flag create');
  return f;
}

/** POST /admin/flags/{name} — toggle only {enabled}. */
export async function toggleFlag(
  api: BoundAdminApi,
  name: string,
  enabled: boolean,
): Promise<FeatureFlag> {
  const raw = await api.post<unknown>(`/admin/flags/${encodeURIComponent(name)}`, { enabled });
  const f = parseFlag(raw);
  if (f === null) throw malformed('flag toggle');
  return f;
}

/** POST /admin/flags/{name}/advance — step the canary ladder. */
export async function advanceFlag(api: BoundAdminApi, name: string): Promise<boolean> {
  const raw = await api.post<unknown>(`/admin/flags/${encodeURIComponent(name)}/advance`, {});
  if (!isRecord(raw)) throw malformed('flag advance');
  return bool(raw['ladder_done']) ?? false;
}

export async function deleteFlag(api: ApiClient, env: AdminEnv, name: string): Promise<void> {
  await api.delete(`/admin/flags/${encodeURIComponent(name)}`, envOpts(env));
}

// ---------------------------------------------------------------------------
// IP bans & allowlist (Task 5.3.34 backend — progressive ban ladder).
// ---------------------------------------------------------------------------

export interface IpBan {
  ip: string;
  level: number;
  strikes: number;
  reason: string;
  bannedAtMs: number;
  expiresAtMs: number;
  actor: string;
}

function parseBan(v: unknown): IpBan | null {
  if (!isRecord(v)) return null;
  const ip = str(v['ip']);
  if (ip === undefined) return null;
  return {
    ip,
    level: num(v['level']) ?? 0,
    strikes: num(v['strikes']) ?? 0,
    reason: str(v['reason']) ?? '',
    bannedAtMs: num(v['banned_at_ms']) ?? 0,
    expiresAtMs: num(v['expires_at_ms']) ?? 0,
    actor: str(v['actor']) ?? '',
  };
}

export async function fetchIpBans(api: BoundAdminApi): Promise<IpBan[]> {
  const raw = await api.get<unknown>('/admin/ip-bans');
  if (!isRecord(raw) || !Array.isArray(raw['bans'])) throw malformed('ban list');
  return (raw['bans'] as unknown[]).map(parseBan).filter((b): b is IpBan => b !== null);
}

/** Ban audit rows arrive as opaque JSON objects — render verbatim. */
export async function fetchIpBanAudit(api: BoundAdminApi, limit = 100): Promise<unknown[]> {
  const raw = await api.get<unknown>('/admin/ip-bans/audit', { limit: String(limit) });
  if (!isRecord(raw) || !Array.isArray(raw['audit'])) throw malformed('ban audit');
  return raw['audit'] as unknown[];
}

export async function banIp(
  api: ApiClient,
  env: AdminEnv,
  input: { ip: string; durationS: number; reason: string },
): Promise<IpBan> {
  const raw = await api.put<unknown>(
    `/admin/ip-bans/${encodeURIComponent(input.ip)}`,
    { duration_s: input.durationS, reason: input.reason },
    envOpts(env),
  );
  const b = parseBan(raw);
  if (b === null) throw malformed('ban response');
  return b;
}

export async function unbanIp(
  api: ApiClient,
  env: AdminEnv,
  ip: string,
  pardon: boolean,
): Promise<void> {
  await api.delete(
    `/admin/ip-bans/${encodeURIComponent(ip)}${pardon ? '?pardon=1' : ''}`,
    envOpts(env),
  );
}

export async function allowlistIp(api: ApiClient, env: AdminEnv, ip: string): Promise<void> {
  await api.put(`/admin/ip-allowlist/${encodeURIComponent(ip)}`, {}, envOpts(env));
}

export async function unallowlistIp(api: ApiClient, env: AdminEnv, ip: string): Promise<void> {
  await api.delete(`/admin/ip-allowlist/${encodeURIComponent(ip)}`, envOpts(env));
}

// ---------------------------------------------------------------------------
// Maintenance windows (Task 5.3.14 backend).
// ---------------------------------------------------------------------------

export const MAINTENANCE_SCOPES = [
  'FULL_VENUE',
  'GATEWAY',
  'MARKET_DATA',
  'SETTLEMENT',
  'FUNDING',
  'INSTRUMENT',
] as const;
export const MAINTENANCE_STATUSES = ['SCHEDULED', 'IN_PROGRESS', 'COMPLETED', 'CANCELLED'] as const;

export interface MaintenanceWindow {
  id: number;
  title: string;
  description?: string;
  scope?: string;
  symbols?: string[];
  status?: string;
  startsAt?: string;
  endsAt?: string;
}

function parseWindow(v: unknown): MaintenanceWindow | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  if (id === undefined) return null;
  return {
    id,
    title: str(v['title']) ?? '',
    description: str(v['description']),
    scope: str(v['scope']),
    symbols: Array.isArray(v['symbols'])
      ? (v['symbols'] as unknown[]).filter((s): s is string => typeof s === 'string')
      : undefined,
    status: str(v['status']),
    startsAt: str(v['starts_at']),
    endsAt: str(v['ends_at']),
  };
}

export async function fetchMaintenanceWindows(api: BoundAdminApi): Promise<MaintenanceWindow[]> {
  const raw = await api.get<unknown>('/admin/maintenance-windows', { limit: '100' });
  if (!isRecord(raw) || !Array.isArray(raw['data'])) throw malformed('maintenance list');
  return (raw['data'] as unknown[])
    .map(parseWindow)
    .filter((w): w is MaintenanceWindow => w !== null);
}

export interface MaintenanceInput {
  title: string;
  description?: string;
  scope?: string;
  symbols?: string[];
  status?: string;
  startsAt: string; // RFC3339
  endsAt: string;
}

export async function createMaintenanceWindow(
  api: BoundAdminApi,
  input: MaintenanceInput,
): Promise<void> {
  await api.post('/admin/maintenance-windows', {
    title: input.title,
    description: input.description,
    scope: input.scope,
    symbols: input.symbols,
    status: input.status,
    starts_at: input.startsAt,
    ends_at: input.endsAt,
  });
}

export async function updateMaintenanceWindow(
  api: ApiClient,
  env: AdminEnv,
  id: number,
  patch: Partial<MaintenanceInput>,
): Promise<void> {
  const body: Record<string, unknown> = {};
  if (patch.title !== undefined) body['title'] = patch.title;
  if (patch.description !== undefined) body['description'] = patch.description;
  if (patch.scope !== undefined) body['scope'] = patch.scope;
  if (patch.symbols !== undefined) body['symbols'] = patch.symbols;
  if (patch.status !== undefined) body['status'] = patch.status;
  if (patch.startsAt !== undefined) body['starts_at'] = patch.startsAt;
  if (patch.endsAt !== undefined) body['ends_at'] = patch.endsAt;
  await api.patch(`/admin/maintenance-windows/${id}`, body, envOpts(env));
}

export async function deleteMaintenanceWindow(
  api: ApiClient,
  env: AdminEnv,
  id: number,
): Promise<void> {
  await api.delete(`/admin/maintenance-windows/${id}`, envOpts(env));
}

// ---------------------------------------------------------------------------
// Destructive trade ops — mass cancel, manual liquidation, cache warm,
// FIX session entitlement update. All typed-confirm + dual-control UX.
// ---------------------------------------------------------------------------

export interface MassCancelInput {
  accountId?: number;
  symbol?: string;
  side?: string;
  orderType?: string;
}

export async function adminMassCancel(
  api: BoundAdminApi,
  input: MassCancelInput,
): Promise<Record<string, unknown>> {
  const body: Record<string, unknown> = {};
  if (input.accountId !== undefined) body['account_id'] = input.accountId;
  if (input.symbol !== undefined) body['symbol'] = input.symbol;
  if (input.side !== undefined) body['side'] = input.side;
  if (input.orderType !== undefined) body['order_type'] = input.orderType;
  const raw = await api.post<unknown>('/admin/orders/mass-cancel', body);
  if (!isRecord(raw)) throw malformed('mass-cancel response');
  return raw;
}

export interface ManualLiquidationInput {
  accountId: number;
  instrumentId?: number;
  reason: string;
  overrideAuction: boolean;
  approverId: number;
}

export async function adminManualLiquidation(
  api: BoundAdminApi,
  input: ManualLiquidationInput,
): Promise<Record<string, unknown>> {
  const body: Record<string, unknown> = {
    account_id: input.accountId,
    reason: input.reason,
    override_auction: input.overrideAuction,
    approver_id: input.approverId,
  };
  if (input.instrumentId !== undefined) body['instrument_id'] = input.instrumentId;
  const raw = await api.post<unknown>('/admin/liquidation/manual', body);
  if (!isRecord(raw)) throw malformed('manual liquidation response');
  return raw;
}

/** POST /admin/cache/warm — synchronous warm; report is the body. */
export async function adminCacheWarm(api: BoundAdminApi): Promise<Record<string, unknown>> {
  const raw = await api.post<unknown>('/admin/cache/warm', {});
  if (!isRecord(raw)) throw malformed('cache-warm report');
  return raw;
}

export interface FixSessionPatch {
  accountId?: number | null;
  apiKeyId?: number | null;
  allowedInstruments?: string | null;
  cancelOnDisconnect?: boolean;
}

export async function updateFixSession(
  api: ApiClient,
  env: AdminEnv,
  sessionId: string,
  patch: FixSessionPatch,
): Promise<void> {
  const body: Record<string, unknown> = {};
  if (patch.accountId !== undefined) body['account_id'] = patch.accountId;
  if (patch.apiKeyId !== undefined) body['api_key_id'] = patch.apiKeyId;
  if (patch.allowedInstruments !== undefined)
    body['allowed_instruments'] = patch.allowedInstruments;
  if (patch.cancelOnDisconnect !== undefined)
    body['cancel_on_disconnect'] = patch.cancelOnDisconnect;
  await api.put(`/admin/fix-sessions/${encodeURIComponent(sessionId)}`, body, envOpts(env));
}
