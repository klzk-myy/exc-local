/**
 * Content, promotions & emergency admin API surface (Phase-10.5 Task
 * 10.5.3.16) — announcements + maintenance windows, financial-promotion
 * lifecycle, strategy-template curation & copy-strategy suspend,
 * break-glass grants, API-key expiry extension (dual-control), LP
 * detail/alerts, and webhook dead-letter retransmission. All calls are
 * environment-bound admin calls.
 */
import type { ApiClient } from '@/lib/api/client';
import { ADMIN_ENV_HEADER, type AdminEnv, type BoundAdminApi } from '@/lib/env';

const envOpts = (env: AdminEnv) => ({ headers: { [ADMIN_ENV_HEADER]: env } });

const record = (v: unknown): Record<string, unknown> =>
  typeof v === 'object' && v !== null ? (v as Record<string, unknown>) : {};
const str = (v: unknown): string => (typeof v === 'string' ? v : '');
const num = (v: unknown): number => (typeof v === 'number' ? v : Number(v ?? 0));
const optStr = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined);
const optNum = (v: unknown): number | undefined =>
  typeof v === 'number' ? v : v === undefined || v === null ? undefined : Number(v);
const boolOf = (v: unknown): boolean => v === true;
const listOf = <T>(v: unknown, fn: (x: unknown) => T): T[] => (Array.isArray(v) ? v.map(fn) : []);

// ---------- announcements & maintenance windows (Task 5.3.14) ----------

export interface Announcement {
  id: number;
  title: string;
  body: string;
  category: string;
  status: string;
  publishAt: string;
  expiresAt?: string;
  createdBy?: string;
}

const announcement = (v: unknown): Announcement => {
  const r = record(v);
  return {
    id: num(r['id']),
    title: str(r['title']),
    body: str(r['body']),
    category: str(r['category']),
    status: str(r['status']),
    publishAt: str(r['publish_at']),
    expiresAt: optStr(r['expires_at']),
    createdBy: optStr(r['created_by']),
  };
};

export function fetchAnnouncements(api: BoundAdminApi): Promise<Announcement[]> {
  return api.get('/admin/announcements').then((r) => listOf(record(r)['data'], announcement));
}

export function createAnnouncement(
  api: BoundAdminApi,
  body: {
    title: string;
    body: string;
    category?: string;
    status?: string;
    publish_at?: string;
    expires_at?: string;
  },
): Promise<Announcement> {
  return api.post('/admin/announcements', body).then(announcement);
}

export function updateAnnouncement(
  api: ApiClient,
  env: AdminEnv,
  id: number,
  body: Record<string, string>,
): Promise<void> {
  return api.patch(`/admin/announcements/${id}`, body, envOpts(env)).then(() => undefined);
}

export function retractAnnouncement(api: ApiClient, env: AdminEnv, id: number): Promise<void> {
  return api.delete(`/admin/announcements/${id}`, envOpts(env)).then(() => undefined);
}

export interface MaintWindow {
  id: number;
  title: string;
  description: string;
  scope: string;
  symbols: string[];
  status: string;
  startsAt: string;
  endsAt: string;
}

const maintWindow = (v: unknown): MaintWindow => {
  const r = record(v);
  return {
    id: num(r['id']),
    title: str(r['title']),
    description: str(r['description']),
    scope: str(r['scope']),
    symbols: listOf(r['symbols'], (x) => str(x)),
    status: str(r['status']),
    startsAt: str(r['starts_at']),
    endsAt: str(r['ends_at']),
  };
};

export function fetchMaintWindows(api: BoundAdminApi): Promise<MaintWindow[]> {
  return api.get('/admin/maintenance-windows').then((r) => listOf(record(r)['data'], maintWindow));
}

export function createMaintWindow(
  api: BoundAdminApi,
  body: {
    title: string;
    description?: string;
    scope?: string;
    symbols?: string[];
    starts_at: string;
    ends_at: string;
  },
): Promise<MaintWindow> {
  return api.post('/admin/maintenance-windows', body).then(maintWindow);
}

export function cancelMaintWindow(api: ApiClient, env: AdminEnv, id: number): Promise<void> {
  return api.delete(`/admin/maintenance-windows/${id}`, envOpts(env)).then(() => undefined);
}

// ---------- financial promotions (Task 21.3.26) ----------

export interface Promotion {
  promotionId: number;
  slug: string;
  channel: string;
  bodyRef: string;
  title: string;
  version: number;
  approvalStatus: string;
  containsClaim: boolean;
  approver?: number;
  secondApprover?: number;
  approvedUntil?: string;
  rejectionReason?: string;
}

const promotion = (v: unknown): Promotion => {
  const r = record(v);
  return {
    promotionId: num(r['promotion_id']),
    slug: str(r['slug']),
    channel: str(r['channel']),
    bodyRef: str(r['body_ref']),
    title: str(r['title']),
    version: num(r['version']),
    approvalStatus: str(r['approval_status']),
    containsClaim: boolOf(r['contains_claim']),
    approver: optNum(r['approver']),
    secondApprover: optNum(r['second_approver']),
    approvedUntil: optStr(r['approved_until']),
    rejectionReason: optStr(r['rejection_reason']),
  };
};

export function fetchPromotions(api: BoundAdminApi, status?: string): Promise<Promotion[]> {
  return api
    .get(`/admin/promotions${status !== undefined && status !== '' ? `?status=${status}` : ''}`)
    .then((r) => listOf(record(r)['promotions'], promotion));
}

export function createPromotion(
  api: BoundAdminApi,
  body: { slug: string; channel: string; body_ref: string; title: string; contains_claim: boolean },
): Promise<void> {
  return api.post('/admin/promotions', body).then(() => undefined);
}

/** PUT lands a new version under the same slug (revision seam). */
export function revisePromotion(
  api: ApiClient,
  env: AdminEnv,
  body: { slug: string; channel: string; body_ref: string; title: string; contains_claim: boolean },
): Promise<void> {
  return api.put('/admin/promotions', body, envOpts(env)).then(() => undefined);
}

export function promotionTransition(
  api: BoundAdminApi,
  id: number,
  verb: 'submit' | 'reject' | 'withdraw',
  body?: { reason?: string },
): Promise<void> {
  return api.post(`/admin/promotions/${id}/${verb}`, body ?? {}).then(() => undefined);
}

/** Approve — claims-carrying promos require a distinct second approver. */
export function approvePromotion(
  api: BoundAdminApi,
  id: number,
  body: {
    checklist: {
      risk_warning: boolean;
      capital_at_risk: boolean;
      claim_basis: boolean;
      entity_details: boolean;
      fair_clear: boolean;
    };
    second_approver_id?: number;
    approved_until?: string;
  },
): Promise<void> {
  return api.post(`/admin/promotions/${id}/approve`, body).then(() => undefined);
}

export interface PromoReport {
  byStatus: Record<string, number>;
  byChannel: Record<string, number>;
  slaBreached: number;
  draftsAging: number;
  expiredFlagged: number;
  expiringIn30d: number;
}

export function fetchPromoReport(api: BoundAdminApi): Promise<PromoReport> {
  return api.get('/admin/promotions/report').then((r) => {
    const p = record(record(r)['promotions']);
    const counts = (v: unknown): Record<string, number> => {
      const out: Record<string, number> = {};
      for (const [k, x] of Object.entries(record(v))) {
        out[k] = num(x);
      }
      return out;
    };
    return {
      byStatus: counts(p['by_status']),
      byChannel: counts(p['by_channel']),
      slaBreached: num(p['sla_breached']),
      draftsAging: num(p['drafts_aging']),
      expiredFlagged: num(p['expired_flagged']),
      expiringIn30d: num(p['expiring_in_30d']),
    };
  });
}

// ---------- strategy templates & copy strategies (Task 14.3.14) ----------

export interface Template {
  templateId: number;
  name: string;
  kind: string;
  status: string;
  publisherAccountId: number;
  rejectReason?: string;
}

const template = (v: unknown): Template => {
  const r = record(v);
  return {
    templateId: num(r['template_id']),
    name: str(r['name']),
    kind: str(r['kind']),
    status: str(r['status']),
    publisherAccountId: num(r['publisher_account_id']),
    rejectReason: optStr(r['reject_reason']),
  };
};

export function fetchTemplates(api: BoundAdminApi, status?: string): Promise<Template[]> {
  return api
    .get(
      `/admin/strategy-templates${status !== undefined && status !== '' ? `?status=${status}` : ''}`,
    )
    .then((r) => listOf(record(r)['templates'], template));
}

export function decideTemplate(
  api: BoundAdminApi,
  id: number,
  approve: boolean,
  reason?: string,
): Promise<void> {
  return api
    .post(`/admin/strategy-templates/${id}/${approve ? 'approve' : 'reject'}`, {
      reason: reason ?? '',
    })
    .then(() => undefined);
}

/** Compliance misconduct action — audit-logged before the status flip. */
export function suspendCopyStrategy(api: BoundAdminApi, id: number, reason: string): Promise<void> {
  return api.post(`/admin/copy/strategies/${id}/suspend`, { reason }).then(() => undefined);
}

// ---------- break-glass, API-key expiry, dead letters, LP detail ----------

export function grantBreakGlass(
  api: BoundAdminApi,
  body: {
    grantee_id: number;
    incident_ref: string;
    reason: string;
    ttl_seconds: number;
    second_approver_id?: number;
    unreachable_approver?: boolean;
  },
): Promise<number> {
  return api.post('/admin/break-glass', body).then((r) => num(record(record(r)['grant'])['id']));
}

export function reviewBreakGlass(api: BoundAdminApi, id: number, notes: string): Promise<void> {
  return api.post(`/admin/break-glass/${id}/review`, { notes }).then(() => undefined);
}

/** PUT → always 202: submits a dual-control request, never applied inline. */
export function extendApiKeyExpiry(
  api: ApiClient,
  env: AdminEnv,
  keyId: number,
  body: { until: string; reason: string },
): Promise<number> {
  return api
    .put(`/admin/api-keys/${keyId}/extend-expiry`, body, envOpts(env))
    .then((r) => num(record(record(r)['request'])['id']));
}

export interface DeadLetter {
  id: number;
  deliveryId: string;
  endpointId: number;
  accountId: number;
  event: string;
  status: string;
  attempts: number;
  maxAttempts: number;
  lastError?: string;
  lastStatusCode?: number;
  createdAt: string;
}

const deadLetter = (v: unknown): DeadLetter => {
  const r = record(v);
  return {
    id: num(r['id']),
    deliveryId: str(r['delivery_id']),
    endpointId: num(r['endpoint_id']),
    accountId: num(r['account_id']),
    event: str(r['event']),
    status: str(r['status']),
    attempts: num(r['attempts']),
    maxAttempts: num(r['max_attempts']),
    lastError: optStr(r['last_error']),
    lastStatusCode: optNum(r['last_status_code']),
    createdAt: str(r['created_at']),
  };
};

export function fetchDeadLetters(api: BoundAdminApi): Promise<DeadLetter[]> {
  return api
    .get('/admin/webhooks/dead-letters')
    .then((r) => listOf(record(r)['dead_letters'], deadLetter));
}

export function retransmitDeadLetter(api: BoundAdminApi, deliveryId: string): Promise<void> {
  return api
    .post(`/admin/webhooks/dead-letters/${deliveryId}/retransmit`, {})
    .then(() => undefined);
}

// ---------- liquidity-provider detail / update / alerts (Task 7.3.9) ----------

export interface LpInstrument {
  instrumentId: number;
  symbol: string;
  enabled: boolean;
  spreadMarkupBidBps: string;
  spreadMarkupAskBps: string;
  skewBps: string;
  stalenessTimeoutMs: number;
}

export interface LpDetail {
  lpId: number;
  name: string;
  status: string;
  connectionType: string;
  fixSessionEnabled: boolean;
  stalenessTimeoutMs: number;
  instruments: LpInstrument[];
}

const lpDetail = (v: unknown): LpDetail => {
  const r = record(v);
  return {
    lpId: num(r['lp_id']),
    name: str(r['name']),
    status: str(r['status']),
    connectionType: str(r['connection_type']),
    fixSessionEnabled: boolOf(r['fix_session_enabled']),
    stalenessTimeoutMs: num(r['staleness_timeout_ms']),
    instruments: listOf(r['instruments'], (x) => {
      const i = record(x);
      return {
        instrumentId: num(i['instrument_id']),
        symbol: str(i['symbol']),
        enabled: boolOf(i['enabled']),
        spreadMarkupBidBps: str(i['spread_markup_bid_bps']),
        spreadMarkupAskBps: str(i['spread_markup_ask_bps']),
        skewBps: str(i['skew_bps']),
        stalenessTimeoutMs: num(i['staleness_timeout_ms']),
      };
    }),
  };
};

export function fetchLpDetail(api: BoundAdminApi, id: number): Promise<LpDetail> {
  return api.get(`/admin/liquidity-providers/${id}`).then(lpDetail);
}

/** Guarded lifecycle update — server enforces the legal transition table. */
export function updateLp(
  api: ApiClient,
  env: AdminEnv,
  id: number,
  body: { status?: string; staleness_timeout_ms?: number; reason: string },
): Promise<void> {
  return api.put(`/admin/liquidity-providers/${id}`, body, envOpts(env)).then(() => undefined);
}

export interface LpAlert {
  id: number;
  lpId: number;
  metric: string;
  observed: number;
  threshold: number;
  window: string;
  status: string;
  emittedAt: string;
}

export function fetchLpAlerts(
  api: BoundAdminApi,
  id: number,
  openOnly: boolean,
): Promise<LpAlert[]> {
  return api.get(`/admin/liquidity-providers/${id}/alerts${openOnly ? '?open=1' : ''}`).then((r) =>
    listOf(record(r)['alerts'], (x) => {
      const a = record(x);
      return {
        id: num(a['id']),
        lpId: num(a['lp_id']),
        metric: str(a['metric']),
        observed: num(a['observed']),
        threshold: num(a['threshold']),
        window: str(a['window']),
        status: str(a['status']),
        emittedAt: str(a['emitted_at']),
      };
    }),
  );
}
