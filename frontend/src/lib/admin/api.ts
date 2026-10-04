/**
 * Admin API wire seam (Phase-10 Tasks 10.3.6 + 10.3.20).
 *
 * Typed fetchers + tolerant parsers for the live admin surface
 * (services/internal/gateway/routes_v1.go Status=Live rows):
 *
 *   GET /api/v1/system/status              public aggregate status
 *   GET /api/v1/admin/ops/health           admin health export (auditor+)
 *   GET /api/v1/admin/audit-log            hash-chained admin audit
 *   GET /api/v1/admin/dual-control         dual-control queue
 *   GET /api/v1/admin/support/tickets      support queue
 *   GET /api/v1/admin/support/accounts/{id} support-view dossier
 *   GET /api/v1/admin/fleet/environments|hosts|topology
 *   GET /api/v1/admin/releases             release registry
 *   POST /api/v1/admin/releases/{id}/promote  (dual-control in prod)
 *   POST /api/v1/admin/fleet/hosts/{id}/{drain|cordon|decommission}
 *
 * Env-scoped admin calls take a `BoundAdminApi` (src/lib/env) so the
 * X-Admin-Env header is stamped by the bound context — a page rendered
 * for staging cannot emit a production call.
 *
 * Previously-stub routes (/admin/ops-board, /admin/instruments*,
 * /account/pnl, …) are all live — the registry carries zero StatusStub
 * rows. Runtime errors still surface as "unavailable" panels per the
 * fail-closed rule.
 */
import type { ApiClient, QueryParams } from '@/lib/api/client';
import { ApiError } from '@/lib/api';
import type { BoundAdminApi } from '@/lib/env';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined);
const num = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined;
const bool = (v: unknown): boolean | undefined => (typeof v === 'boolean' ? v : undefined);
const arr = (v: unknown): unknown[] => (Array.isArray(v) ? v : []);

export function malformed(what: string): ApiError {
  return new ApiError({
    type: 'error',
    error: 'INVALID_REQUEST',
    message: `malformed ${what} payload`,
    status: 0,
  });
}

/** A list-envelope page (NewListEnvelope — data/next_cursor/limit/total). */
export interface ListPage<T> {
  data: T[];
  nextCursor: string;
  limit: number;
  total: number;
}

function parseListPage<T>(v: unknown, item: (x: unknown) => T | null): ListPage<T> | null {
  if (!isRecord(v) || !Array.isArray(v['data'])) return null;
  const data: T[] = [];
  for (const it of v['data']) {
    const p = item(it);
    if (p !== null) data.push(p);
  }
  return {
    data,
    nextCursor: str(v['next_cursor']) ?? '',
    limit: num(v['limit']) ?? data.length,
    total: num(v['total']) ?? data.length,
  };
}

// ---------------------------------------------------------------------------
// System status + admin health (ops status shapes — Task 10.3.6/10.3.20)
// ---------------------------------------------------------------------------

export interface ComponentHealth {
  name: string;
  state: string; // operational | degraded | down | unknown
  critical: boolean;
  latencyMs: number;
  detail?: string;
}

/** ops.Status — GET /api/v1/system/status. `source` distinguishes the
 * aggregator document from the truthful gateway-local fallback. */
export interface SystemStatus {
  status: string;
  mode: string;
  components: ComponentHealth[];
  metrics: Record<string, number>;
  source: string; // "aggregator" | "gateway-local"
  updatedAt: string;
  detail?: string;
}

export function parseSystemStatus(v: unknown): SystemStatus | null {
  if (!isRecord(v)) return null;
  const status = str(v['status']);
  const source = str(v['source']);
  if (status === undefined || source === undefined) return null;
  const components: ComponentHealth[] = [];
  for (const c of arr(v['components'])) {
    if (!isRecord(c)) continue;
    const name = str(c['name']);
    if (name === undefined) continue;
    components.push({
      name,
      state: str(c['state']) ?? 'unknown',
      critical: bool(c['critical']) ?? false,
      latencyMs: num(c['latency_ms']) ?? 0,
      detail: str(c['detail']),
    });
  }
  const metrics: Record<string, number> = {};
  if (isRecord(v['metrics'])) {
    for (const [k, mv] of Object.entries(v['metrics'])) {
      const n = num(mv);
      if (n !== undefined) metrics[k] = n;
    }
  }
  return {
    status,
    mode: str(v['mode']) ?? 'unknown',
    components,
    metrics,
    source,
    updatedAt: str(v['updated_at']) ?? '',
    detail: str(v['detail']),
  };
}

/** GET /api/v1/system/status — public; no admin env header applies. */
export async function fetchSystemStatus(api: ApiClient): Promise<SystemStatus> {
  const res = parseSystemStatus(await api.get<unknown>('/system/status'));
  if (res === null) throw malformed('system status');
  return res;
}

/** AdminOpsHealth — GET /api/v1/admin/ops/health. The handler emits a
 * dynamic map (status / mode / components hash-map / recent_events /
 * uptime / load_shedding / warm_state); fields stay `unknown`-ish and
 * renderers format defensively. */
export interface OpsHealth {
  status: unknown;
  mode?: string;
  modeSinceMs?: number;
  modeReason?: string;
  /** status:component:* hash — name → redis hash fields (strings). */
  components: Record<string, Record<string, string>>;
  recentEvents: unknown[];
  uptime?: Record<string, unknown>;
  loadShedding?: unknown;
  warmState?: Record<string, unknown>;
}

export function parseOpsHealth(v: unknown): OpsHealth | null {
  if (!isRecord(v)) return null;
  const components: Record<string, Record<string, string>> = {};
  if (isRecord(v['components'])) {
    for (const [name, fields] of Object.entries(v['components'])) {
      if (!isRecord(fields)) continue;
      const row: Record<string, string> = {};
      for (const [fk, fv] of Object.entries(fields)) {
        if (typeof fv === 'string') row[fk] = fv;
      }
      components[name] = row;
    }
  }
  return {
    status: v['status'],
    mode: str(v['mode']),
    modeSinceMs: num(v['mode_since_ms']),
    modeReason: str(v['mode_reason']),
    components,
    recentEvents: arr(v['recent_events']),
    uptime: isRecord(v['uptime']) ? v['uptime'] : undefined,
    loadShedding: v['load_shedding'],
    warmState: isRecord(v['warm_state']) ? v['warm_state'] : undefined,
  };
}

export async function fetchOpsHealth(api: BoundAdminApi): Promise<OpsHealth> {
  const res = parseOpsHealth(await api.get<unknown>('/admin/ops/health'));
  if (res === null) throw malformed('ops health');
  return res;
}

// ---------------------------------------------------------------------------
// Market-ops board (GET /api/v1/admin/ops-board — Phase-15 Task 15.3.12,
// Risk Manager). Read-only consolidated board: non-ACTIVE instruments
// with grace windows + engine-status drift, pending listing proposals,
// pending four-eyes approvals, upcoming auctions, today's fixings, and
// operational warnings.
// ---------------------------------------------------------------------------

export interface OpsBoardInstrument {
  symbol: string;
  status: string;
  engineStatus?: string;
  statusDrift?: boolean;
  graceKind?: string;
  graceDeadline?: string;
  delistPhase?: string;
}

export interface OpsBoardProposal {
  id: number;
  symbol: string;
  status: string;
  overdue?: boolean;
  createdAt?: string;
}

export interface OpsBoardApproval {
  id: number;
  operation: string;
  targetId: string;
  requiredRole?: string;
  expiresAt?: string;
}

export interface OpsBoardAuction {
  symbol: string;
  auctionType: string;
  benchmark?: string;
  nextAt?: string;
}

export interface OpsBoardFixing {
  symbol: string;
  benchmark: string;
  status: string;
  scheduledAt?: string;
  rate?: string;
}

export interface OpsBoard {
  generatedAt?: string;
  instruments: OpsBoardInstrument[];
  pendingProposals: OpsBoardProposal[];
  pendingApprovals: OpsBoardApproval[];
  upcomingAuctions: OpsBoardAuction[];
  todayFixings: OpsBoardFixing[];
  warnings: string[];
}

function parseBoardInstrument(v: unknown): OpsBoardInstrument | null {
  if (!isRecord(v)) return null;
  const symbol = str(v['symbol']);
  if (symbol === undefined) return null;
  return {
    symbol,
    status: str(v['status']) ?? 'UNKNOWN',
    engineStatus: str(v['engine_status']),
    statusDrift: bool(v['status_drift']),
    graceKind: str(v['grace_kind']),
    graceDeadline: str(v['grace_deadline']),
    delistPhase: str(v['delist_phase']),
  };
}

function parseBoardProposal(v: unknown): OpsBoardProposal | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  const symbol = str(v['symbol']);
  if (id === undefined || symbol === undefined) return null;
  return {
    id,
    symbol,
    status: str(v['status']) ?? 'UNKNOWN',
    overdue: bool(v['overdue']),
    createdAt: str(v['created_at']),
  };
}

function parseBoardApproval(v: unknown): OpsBoardApproval | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  if (id === undefined) return null;
  return {
    id,
    operation: str(v['operation']) ?? 'UNKNOWN',
    targetId: str(v['target_id']) ?? '',
    requiredRole: str(v['required_role']),
    expiresAt: str(v['expires_at']),
  };
}

function parseBoardAuction(v: unknown): OpsBoardAuction | null {
  if (!isRecord(v)) return null;
  const symbol = str(v['symbol']);
  if (symbol === undefined) return null;
  return {
    symbol,
    auctionType: str(v['auction_type']) ?? 'UNKNOWN',
    benchmark: str(v['benchmark']),
    nextAt: str(v['next_at']),
  };
}

function parseBoardFixing(v: unknown): OpsBoardFixing | null {
  if (!isRecord(v)) return null;
  const symbol = str(v['symbol']);
  const benchmark = str(v['benchmark']);
  if (symbol === undefined || benchmark === undefined) return null;
  return {
    symbol,
    benchmark,
    status: str(v['status']) ?? 'UNKNOWN',
    scheduledAt: str(v['scheduled_at']),
    rate: str(v['rate']),
  };
}

export async function fetchOpsBoard(api: BoundAdminApi): Promise<OpsBoard> {
  const v = await api.get<unknown>('/admin/ops-board');
  if (!isRecord(v)) throw malformed('ops board');
  const pick = <T>(key: string, parse: (row: unknown) => T | null): T[] =>
    arr(v[key])
      .map(parse)
      .filter((r): r is T => r !== null);
  return {
    generatedAt: str(v['generated_at']),
    instruments: pick('instruments', parseBoardInstrument),
    pendingProposals: pick('pending_proposals', parseBoardProposal),
    pendingApprovals: pick('pending_approvals', parseBoardApproval),
    upcomingAuctions: pick('upcoming_auctions', parseBoardAuction),
    todayFixings: pick('today_fixings', parseBoardFixing),
    warnings: arr(v['warnings']).filter((w): w is string => typeof w === 'string'),
  };
}

// ---------------------------------------------------------------------------
// Audit log (GET /api/v1/admin/audit-log — keyset cursor)
// ---------------------------------------------------------------------------

export interface AuditRow {
  id: number;
  adminUserId: number;
  action: string;
  targetType?: string;
  targetId?: number;
  beforeState?: unknown;
  afterState?: unknown;
  ipAddress?: string;
  createdAt: string;
  auditSeq?: number;
}

function parseAuditRow(v: unknown): AuditRow | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  const action = str(v['action']);
  const createdAt = str(v['created_at']);
  if (id === undefined || action === undefined || createdAt === undefined) return null;
  return {
    id,
    adminUserId: num(v['admin_user_id']) ?? 0,
    action,
    targetType: str(v['target_type']),
    targetId: num(v['target_id']),
    beforeState: v['before_state'],
    afterState: v['after_state'],
    ipAddress: str(v['ip_address']),
    createdAt,
    auditSeq: num(v['audit_seq']),
  };
}

export interface AuditQuery {
  action?: string;
  actionPrefix?: string;
  adminUserId?: number;
  targetType?: string;
  targetId?: number;
  from?: string; // RFC3339
  to?: string;
  cursor?: string;
  limit?: number;
}

export async function fetchAuditLog(
  api: BoundAdminApi,
  q: AuditQuery,
): Promise<ListPage<AuditRow>> {
  const query: QueryParams = {
    action: q.action,
    action_prefix: q.actionPrefix,
    admin_user_id: q.adminUserId,
    target_type: q.targetType,
    target_id: q.targetId,
    from: q.from,
    to: q.to,
    cursor: q.cursor,
    limit: q.limit ?? 50,
  };
  const page = parseListPage(await api.get<unknown>('/admin/audit-log', query), parseAuditRow);
  if (page === null) throw malformed('audit log');
  return page;
}

// ---------------------------------------------------------------------------
// Dual-control queue (GET /api/v1/admin/dual-control, approve/reject)
// ---------------------------------------------------------------------------

export interface DualControlRequest {
  id: number;
  operation: string;
  targetType: string;
  targetId: string;
  payload?: unknown;
  requiredRole: string;
  requestedBy: number;
  approvedBy?: number;
  status: string; // PENDING | APPROVED | REJECTED | EXPIRED | EXECUTED
  reason?: string;
  createdAt: string;
  expiresAt: string;
  decidedAt?: string;
}

function parseDualRow(v: unknown): DualControlRequest | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  const operation = str(v['operation']);
  if (id === undefined || operation === undefined) return null;
  return {
    id,
    operation,
    targetType: str(v['target_type']) ?? '',
    targetId: str(v['target_id']) ?? num(v['target_id'])?.toString() ?? '',
    payload: v['payload'],
    requiredRole: str(v['required_role']) ?? '',
    requestedBy: num(v['requested_by']) ?? 0,
    approvedBy: num(v['approved_by']),
    status: str(v['status']) ?? 'PENDING',
    reason: str(v['reason']),
    createdAt: str(v['created_at']) ?? '',
    expiresAt: str(v['expires_at']) ?? '',
    decidedAt: str(v['decided_at']),
  };
}

export async function fetchDualControl(
  api: BoundAdminApi,
  status?: string,
  limit = 100,
): Promise<DualControlRequest[]> {
  const raw = await api.get<unknown>('/admin/dual-control', { status, limit });
  if (!isRecord(raw) || !Array.isArray(raw['requests'])) throw malformed('dual-control');
  const out: DualControlRequest[] = [];
  for (const r of raw['requests']) {
    const p = parseDualRow(r);
    if (p !== null) out.push(p);
  }
  return out;
}

export async function decideDualControl(
  api: BoundAdminApi,
  id: number,
  approve: boolean,
): Promise<DualControlRequest | null> {
  const raw = await api.post<unknown>(
    `/admin/dual-control/${id}/${approve ? 'approve' : 'reject'}`,
    {},
  );
  if (isRecord(raw) && raw['request'] !== undefined) return parseDualRow(raw['request']);
  return null;
}

// ---------------------------------------------------------------------------
// Support queue + support-view dossier
// ---------------------------------------------------------------------------

export interface SupportTicket {
  ticketId: number;
  accountId: number;
  type: string;
  category: string;
  priority: string;
  subject: string;
  status: string;
  queue: string;
  assigneeAdminId?: number;
  slaDueAt?: string;
  slaBreached: boolean;
  createdAt: string;
  updatedAt: string;
}

function parseTicket(v: unknown): SupportTicket | null {
  if (!isRecord(v)) return null;
  const id = num(v['ticket_id']);
  const subject = str(v['subject']);
  if (id === undefined || subject === undefined) return null;
  return {
    ticketId: id,
    accountId: num(v['account_id']) ?? 0,
    type: str(v['type']) ?? '',
    category: str(v['category']) ?? '',
    priority: str(v['priority']) ?? '',
    subject,
    status: str(v['status']) ?? '',
    queue: str(v['queue']) ?? '',
    assigneeAdminId: num(v['assignee_admin_id']),
    slaDueAt: str(v['sla_due_at']),
    slaBreached: bool(v['sla_breached']) ?? false,
    createdAt: str(v['created_at']) ?? '',
    updatedAt: str(v['updated_at']) ?? '',
  };
}

export async function fetchSupportTickets(
  api: BoundAdminApi,
  q: { status?: string; queue?: string; cursor?: string; limit?: number } = {},
): Promise<ListPage<SupportTicket>> {
  const raw = await api.get<unknown>('/admin/support/tickets', {
    status: q.status,
    queue: q.queue,
    cursor: q.cursor,
    limit: q.limit ?? 50,
  });
  const page = parseListPage(raw, parseTicket);
  if (page === null) throw malformed('support tickets');
  return page;
}

/** Support-view dossier (admin.SupportView) — read-only, audit-logged
 * server-side; Support Agent / Super Admin only. */
export interface SupportView {
  accountId: number;
  userId: number;
  status: string;
  kycTier: string;
  accountType: string;
  createdAt: string;
  balances: { currency: string; available: string; locked: string }[];
  recentOrders: {
    id: number;
    symbol?: string;
    side: string;
    orderType: string;
    status: string;
    quantity: string;
    filledQuantity: string;
    price?: string;
    createdAt: string;
  }[];
  recentTickets: {
    id: number;
    type: string;
    category: string;
    status: string;
    priority: string;
    subject: string;
    createdAt: string;
  }[];
  kycDocuments: { id: number; type: string; status: string; createdAt: string }[];
}

export function parseSupportView(v: unknown): SupportView | null {
  if (!isRecord(v)) return null;
  const accountId = num(v['account_id']);
  if (accountId === undefined) return null;
  const balances: SupportView['balances'] = [];
  for (const b of arr(v['balances'])) {
    if (!isRecord(b)) continue;
    const currency = str(b['currency']);
    if (currency === undefined) continue;
    balances.push({
      currency,
      available: str(b['available']) ?? '0',
      locked: str(b['locked']) ?? '0',
    });
  }
  const recentOrders: SupportView['recentOrders'] = [];
  for (const o of arr(v['recent_orders'])) {
    if (!isRecord(o)) continue;
    const id = num(o['id']);
    if (id === undefined) continue;
    recentOrders.push({
      id,
      symbol: str(o['symbol']),
      side: str(o['side']) ?? '',
      orderType: str(o['order_type']) ?? '',
      status: str(o['status']) ?? '',
      quantity: str(o['quantity']) ?? '0',
      filledQuantity: str(o['filled_quantity']) ?? '0',
      price: str(o['price']),
      createdAt: str(o['created_at']) ?? '',
    });
  }
  const recentTickets: SupportView['recentTickets'] = [];
  for (const t of arr(v['recent_tickets'])) {
    if (!isRecord(t)) continue;
    const id = num(t['id']);
    if (id === undefined) continue;
    recentTickets.push({
      id,
      type: str(t['type']) ?? '',
      category: str(t['category']) ?? '',
      status: str(t['status']) ?? '',
      priority: str(t['priority']) ?? '',
      subject: str(t['subject']) ?? '',
      createdAt: str(t['created_at']) ?? '',
    });
  }
  const kycDocuments: SupportView['kycDocuments'] = [];
  for (const d of arr(v['kyc_documents'])) {
    if (!isRecord(d)) continue;
    const id = num(d['id']);
    if (id === undefined) continue;
    kycDocuments.push({
      id,
      type: str(d['type']) ?? '',
      status: str(d['status']) ?? '',
      createdAt: str(d['created_at']) ?? '',
    });
  }
  return {
    accountId,
    userId: num(v['user_id']) ?? 0,
    status: str(v['status']) ?? '',
    kycTier: str(v['kyc_tier']) ?? '',
    accountType: str(v['account_type']) ?? '',
    createdAt: str(v['created_at']) ?? '',
    balances,
    recentOrders,
    recentTickets,
    kycDocuments,
  };
}

export async function fetchSupportAccount(
  api: BoundAdminApi,
  accountId: number,
): Promise<SupportView> {
  const raw = await api.get<unknown>(`/admin/support/accounts/${accountId}`);
  const view = parseSupportView(raw);
  if (view === null) throw malformed('support view');
  return view;
}

// ---------------------------------------------------------------------------
// Fleet (env-scoped via BoundAdminApi)
// ---------------------------------------------------------------------------

export interface FleetEnvironment {
  id: number;
  name: string;
  promotionPolicy?: unknown;
  topologyProfile?: unknown;
}

export interface FleetEnvironments {
  environments: FleetEnvironment[];
  sessionEnv: string;
}

export async function fetchFleetEnvironments(api: BoundAdminApi): Promise<FleetEnvironments> {
  const raw = await api.get<unknown>('/admin/fleet/environments');
  if (!isRecord(raw)) throw malformed('fleet environments');
  const environments: FleetEnvironment[] = [];
  for (const e of arr(raw['environments'])) {
    if (!isRecord(e)) continue;
    const id = num(e['id']);
    const name = str(e['name']);
    if (id === undefined || name === undefined) continue;
    environments.push({
      id,
      name,
      promotionPolicy: e['promotion_policy'],
      topologyProfile: e['topology_profile'],
    });
  }
  return { environments, sessionEnv: str(raw['session_env']) ?? '' };
}

export interface FleetHost {
  id: number;
  env: string;
  hostname: string;
  role: string;
  shardId?: number;
  az?: string;
  rack?: string;
  health: string;
  state: string;
  createdAt: string;
  updatedAt: string;
}

export function parseFleetHost(v: unknown): FleetHost | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  const hostname = str(v['hostname']);
  if (id === undefined || hostname === undefined) return null;
  return {
    id,
    env: str(v['env']) ?? '',
    hostname,
    role: str(v['role']) ?? '',
    shardId: num(v['shard_id']),
    az: str(v['az']),
    rack: str(v['rack']),
    health: str(v['health']) ?? 'unknown',
    state: str(v['state']) ?? 'unknown',
    createdAt: str(v['created_at']) ?? '',
    updatedAt: str(v['updated_at']) ?? '',
  };
}

export async function fetchFleetHosts(
  api: BoundAdminApi,
  q: { state?: string; role?: string } = {},
): Promise<FleetHost[]> {
  const raw = await api.get<unknown>('/admin/fleet/hosts', { state: q.state, role: q.role });
  if (!isRecord(raw) || !Array.isArray(raw['hosts'])) throw malformed('fleet hosts');
  const out: FleetHost[] = [];
  for (const h of raw['hosts']) {
    const p = parseFleetHost(h);
    if (p !== null) out.push(p);
  }
  return out;
}

/** fleet.TopologyView — env + shard→hosts + role→hostnames + health counts. */
export interface TopologyView {
  env: string;
  shards: Record<string, FleetHost[]>;
  roles: Record<string, string[]>;
  health: Record<string, number>;
}

export async function fetchFleetTopology(api: BoundAdminApi, env?: string): Promise<TopologyView> {
  const raw = await api.get<unknown>('/admin/fleet/topology', { env });
  if (!isRecord(raw)) throw malformed('fleet topology');
  const shards: Record<string, FleetHost[]> = {};
  if (isRecord(raw['shards'])) {
    for (const [k, hosts] of Object.entries(raw['shards'])) {
      const list: FleetHost[] = [];
      for (const h of arr(hosts)) {
        const p = parseFleetHost(h);
        if (p !== null) list.push(p);
      }
      shards[k] = list;
    }
  }
  const roles: Record<string, string[]> = {};
  if (isRecord(raw['roles'])) {
    for (const [k, names] of Object.entries(raw['roles'])) {
      roles[k] = arr(names).filter((n): n is string => typeof n === 'string');
    }
  }
  const health: Record<string, number> = {};
  if (isRecord(raw['health'])) {
    for (const [k, count] of Object.entries(raw['health'])) {
      const n = num(count);
      if (n !== undefined) health[k] = n;
    }
  }
  return { env: str(raw['env']) ?? '', shards, roles, health };
}

export interface ServerAction {
  id: number;
  hostId: number;
  action: string;
  status: string;
  requestedBy: number;
  approvedBy?: number;
  reason: string;
}

/** POST /api/v1/admin/fleet/hosts/{id}/{drain|cordon|decommission} —
 * dual-control action in production (approver_id required by the
 * service; surfaced via DUAL_CONTROL_REQUIRED errors). */
export async function fleetHostAction(
  api: BoundAdminApi,
  hostId: number,
  action: 'drain' | 'cordon' | 'decommission',
  input: { reason: string; approverId?: number },
): Promise<ServerAction | null> {
  const raw = await api.post<unknown>(`/admin/fleet/hosts/${hostId}/${action}`, {
    reason: input.reason,
    approver_id: input.approverId,
  });
  if (!isRecord(raw)) return null;
  const sa = raw['server_action'];
  if (!isRecord(sa)) return null;
  return {
    id: num(sa['id']) ?? 0,
    hostId: num(sa['host_id']) ?? hostId,
    action: str(sa['action']) ?? action,
    status: str(sa['status']) ?? 'PENDING',
    requestedBy: num(sa['requested_by']) ?? 0,
    approvedBy: num(sa['approved_by']),
    reason: str(sa['reason']) ?? input.reason,
  };
}

// ---------------------------------------------------------------------------
// Releases
// ---------------------------------------------------------------------------

export interface Release {
  id: number;
  component: string;
  version: string;
  artifactHash: string;
  env: string;
  status: string;
  gateEvidence?: unknown;
  notes?: string;
  createdBy: number;
  createdAt: string;
  updatedAt: string;
}

export function parseRelease(v: unknown): Release | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  const component = str(v['component']);
  const version = str(v['version']);
  if (id === undefined || component === undefined || version === undefined) return null;
  return {
    id,
    component,
    version,
    artifactHash: str(v['artifact_hash']) ?? '',
    env: str(v['env']) ?? '',
    status: str(v['status']) ?? 'PENDING',
    gateEvidence: v['gate_evidence'],
    notes: str(v['notes']),
    createdBy: num(v['created_by']) ?? 0,
    createdAt: str(v['created_at']) ?? '',
    updatedAt: str(v['updated_at']) ?? '',
  };
}

export async function fetchReleases(
  api: BoundAdminApi,
  q: { env?: string; status?: string } = {},
): Promise<Release[]> {
  const raw = await api.get<unknown>('/admin/releases', { env: q.env, status: q.status });
  if (!isRecord(raw) || !Array.isArray(raw['releases'])) throw malformed('releases');
  const out: Release[] = [];
  for (const r of raw['releases']) {
    const p = parseRelease(r);
    if (p !== null) out.push(p);
  }
  return out;
}

/** POST /api/v1/admin/releases — register an artifact; dev releases
 * land DEPLOYED (dev auto-deploys, §19.16.3). */
export async function registerRelease(
  api: BoundAdminApi,
  input: {
    component: string;
    version: string;
    artifactHash: string;
    gateEvidence?: unknown;
    notes?: string;
  },
): Promise<Release> {
  const raw = await api.post<unknown>('/admin/releases', {
    component: input.component,
    version: input.version,
    artifact_hash: input.artifactHash,
    gate_evidence: input.gateEvidence,
    notes: input.notes,
  });
  const rel = isRecord(raw) ? parseRelease(raw['release']) : null;
  if (rel === null) throw malformed('release create');
  return rel;
}

/** One evaluated §19.16.3 promotion gate (server: fleet.GateResult). */
export interface GateResult {
  name: string;
  passed: boolean;
  detail?: string;
}

/** Parse a gates array — arrives both in the 200 {promotion, gates}
 * body and inside error.details.gates on a FORBIDDEN/BLOCKED attempt. */
export function parseGates(v: unknown): GateResult[] {
  if (!Array.isArray(v)) return [];
  const out: GateResult[] = [];
  for (const g of v) {
    if (!isRecord(g)) continue;
    const name = str(g['name']);
    if (name === undefined) continue;
    out.push({ name, passed: g['passed'] === true, detail: str(g['detail']) });
  }
  return out;
}

export interface PromoteResult {
  promotionId?: number;
  status: string;
  gates: GateResult[];
}

/** POST /api/v1/admin/releases/{id}/promote — {to_env, reason,
 * approver_id}; prod promotes are dual-control + §19.16.3 gated.
 * Returns the evaluated gate list verbatim — the UI renders it
 * whether the promotion EXECUTED or the request came back BLOCKED. */
export async function promoteRelease(
  api: BoundAdminApi,
  releaseId: number,
  input: { toEnv: string; reason?: string; approverId?: number },
): Promise<PromoteResult> {
  const raw = await api.post<unknown>(`/admin/releases/${releaseId}/promote`, {
    to_env: input.toEnv,
    reason: input.reason,
    approver_id: input.approverId,
  });
  if (!isRecord(raw)) throw malformed('promotion response');
  const promo = isRecord(raw['promotion']) ? raw['promotion'] : {};
  return {
    promotionId: num(promo['id']),
    status: str(promo['status']) ?? 'PENDING',
    gates: parseGates(raw['gates']),
  };
}
