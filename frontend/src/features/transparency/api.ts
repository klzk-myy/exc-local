/**
 * Public transparency & status adapters (Task 10.5.3.23) — every route
 * here is TierPublic (no auth required).
 *
 *   GET /exchange-info          venue document (ETag'd; /venue/info alias)
 *   GET /venue/best-execution/{rts27,rts28}[/{id}[/csv]]  PUBLISHED only
 *   GET /system/incidents       public incident notices
 *   GET /maintenance/schedule   upcoming SCHEDULED/IN_PROGRESS windows
 *   GET /session/status         24/5 session machine
 *   GET /meta/{pagination,rate-limits}  published API contracts
 *   GET /security/policy        text/markdown VDP policy
 *   POST /security/disclosures  public vulnerability intake (201)
 */
import { apiClient } from '@/app/runtime';
import { ApiError, parseErrorEnvelope, type ApiClient } from '@/lib/api';
import { downloadFile } from '@/lib/input-helpers';

type Api = Pick<ApiClient, 'get' | 'post'>;

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined);
const num = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined;
const bool = (v: unknown): boolean | undefined => (typeof v === 'boolean' ? v : undefined);

// ---------------------------------------------------------------------------
// GET /exchange-info — the venue document.
// ---------------------------------------------------------------------------

export interface VenueSymbol {
  symbol?: string;
  instrument_type?: string;
  status?: string;
  tick_size?: string;
  lot_size?: string;
  min_order_qty?: string;
  max_order_qty?: string;
  min_notional?: string;
  max_leverage?: number;
  settlement_cycle?: number;
  settlement?: string;
  order_types?: string[];
  trading_hours?: { type?: string; weekly_open_utc?: string; weekly_close_utc?: string };
  permissions?: { new_orders_allowed?: boolean; market_orders_allowed?: boolean };
}

export interface VenueDoc {
  timezone?: string;
  server_time_ms?: number;
  trading_hours?: {
    type?: string;
    weekly_open_utc?: string;
    weekly_close_utc?: string;
    daily_break_utc?: string;
  };
  symbols: VenueSymbol[];
  product_profiles?: {
    code?: string;
    pricing_plan?: string;
    instrument_scope?: string;
    min_deposit?: string;
  }[];
  leverage_policies?: {
    entity_code?: string;
    client_category?: string;
    instrument_group?: string;
    max_leverage?: number;
    effective_from?: string;
  }[];
}

export async function fetchVenueInfo(api: Api = apiClient): Promise<VenueDoc> {
  const v = await api.get<unknown>('/exchange-info');
  const r = isRecord(v) ? v : {};
  return {
    timezone: str(r['timezone']),
    server_time_ms: num(r['server_time_ms']),
    trading_hours: isRecord(r['trading_hours']) ? r['trading_hours'] : undefined,
    symbols: Array.isArray(r['symbols']) ? (r['symbols'] as VenueSymbol[]) : [],
    product_profiles: Array.isArray(r['product_profiles'])
      ? (r['product_profiles'] as VenueDoc['product_profiles'])
      : undefined,
    leverage_policies: Array.isArray(r['leverage_policies'])
      ? (r['leverage_policies'] as VenueDoc['leverage_policies'])
      : undefined,
  };
}

// ---------------------------------------------------------------------------
// RTS 27/28 — published rows only; the CSV artifact rides /{id}/csv.
// ---------------------------------------------------------------------------

export interface BestExecReport {
  id: number;
  quarter_start?: string; // RTS27
  year?: number; // RTS28
  instrument_class?: string;
  version?: number;
  status?: string;
  days_covered?: number; // RTS27
  zero_activity?: boolean; // RTS27
  qualitative_assessment?: string; // RTS28
  published_at?: string;
}

function parseBestExec(v: unknown): BestExecReport | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  if (id === undefined) return null;
  return {
    id,
    quarter_start: str(v['quarter_start']),
    year: num(v['year']),
    instrument_class: str(v['instrument_class']),
    version: num(v['version']),
    status: str(v['status']),
    days_covered: num(v['days_covered']),
    zero_activity: bool(v['zero_activity']),
    qualitative_assessment: str(v['qualitative_assessment']),
    published_at: str(v['published_at']),
  };
}

export async function fetchBestExec(
  kind: 'rts27' | 'rts28',
  api: Api = apiClient,
): Promise<BestExecReport[]> {
  const res = await api.get<unknown>(`/venue/best-execution/${kind}`);
  const raw = isRecord(res) && Array.isArray(res['reports']) ? res['reports'] : [];
  return raw.map(parseBestExec).filter((r): r is BestExecReport => r !== null);
}

/** Download the published CSV artifact for a report. */
export function downloadBestExecCsv(kind: 'rts27' | 'rts28', id: number) {
  return downloadFile(`/venue/best-execution/${kind}/${id}/csv`, `${kind}-${id}.csv`, {
    accept: 'text/csv',
  });
}

// ---------------------------------------------------------------------------
// Status surfaces — incidents, maintenance, session machine.
// ---------------------------------------------------------------------------

export interface Incident {
  id: number;
  title?: string;
  severity?: string;
  status?: string;
  mode?: string;
  started_at?: string;
  resolved_at?: string;
  summary?: string;
  postmortem_url?: string;
}

export async function fetchIncidents(api: Api = apiClient): Promise<Incident[]> {
  const res = await api.get<unknown>('/system/incidents');
  const raw = isRecord(res) && Array.isArray(res['incidents']) ? res['incidents'] : [];
  return raw.filter(isRecord).map((r) => ({
    id: num(r['id']) ?? 0,
    title: str(r['title']),
    severity: str(r['severity']),
    status: str(r['status']),
    mode: str(r['mode']),
    started_at: str(r['started_at']),
    resolved_at: str(r['resolved_at']),
    summary: str(r['summary']),
    postmortem_url: str(r['postmortem_url']),
  }));
}

export interface MaintWindow {
  id: number;
  title?: string;
  description?: string;
  scope?: string;
  symbols?: string[];
  status?: string;
  starts_at?: string;
  ends_at?: string;
}

export async function fetchMaintenance(api: Api = apiClient): Promise<MaintWindow[]> {
  const res = await api.get<unknown>('/maintenance/schedule');
  const raw = isRecord(res) && Array.isArray(res['data']) ? res['data'] : [];
  return raw.filter(isRecord).map((r) => ({
    id: num(r['id']) ?? 0,
    title: str(r['title']),
    description: str(r['description']),
    scope: str(r['scope']),
    symbols: Array.isArray(r['symbols'])
      ? (r['symbols'] as unknown[]).filter((x): x is string => typeof x === 'string')
      : undefined,
    status: str(r['status']),
    starts_at: str(r['starts_at']),
    ends_at: str(r['ends_at']),
  }));
}

export interface SessionStatusDoc {
  state?: string;
  consistent?: boolean;
  shard_coverage?: string;
  market_open?: boolean;
  next_state?: string;
  next_transition_at?: string;
  shards?: { shard_id?: number; state?: string; reachable?: boolean }[];
  pending_effects?: string[];
}

export async function fetchSessionStatus(api: Api = apiClient): Promise<SessionStatusDoc> {
  const v = await api.get<unknown>('/session/status');
  return isRecord(v) ? v : {};
}

// ---------------------------------------------------------------------------
// Meta docs — pagination matrix + rate-limit table.
// ---------------------------------------------------------------------------

export interface PaginationMeta {
  envelope?: { shape?: string; cursor_order?: string; cursor_opaque?: boolean };
  endpoints: {
    path?: string;
    default_limit?: number;
    max_limit?: number;
    sortable?: string[];
    filterable?: string[];
  }[];
}

export async function fetchPaginationMeta(api: Api = apiClient): Promise<PaginationMeta> {
  const v = await api.get<unknown>('/meta/pagination');
  const r = isRecord(v) ? v : {};
  return {
    envelope: isRecord(r['envelope']) ? r['envelope'] : undefined,
    endpoints: Array.isArray(r['endpoints']) ? (r['endpoints'] as PaginationMeta['endpoints']) : [],
  };
}

export interface RateLimitsDoc {
  tiers?: Record<
    string,
    { rate_per_sec?: number; burst_factor?: number; weight_per_min?: number; keyed_by?: string }
  >;
  counters?: string[];
  bans?: Record<string, unknown>;
  server_time_ms?: number;
}

export async function fetchRateLimitsMeta(api: Api = apiClient): Promise<RateLimitsDoc> {
  const v = await api.get<unknown>('/meta/rate-limits');
  return isRecord(v) ? v : {};
}

// ---------------------------------------------------------------------------
// VDP — policy is served as text/markdown (not JSON); disclosures POST.
// ---------------------------------------------------------------------------

/** GET /security/policy — served as text/markdown, not JSON, so it
 * bypasses ApiClient's JSON path (same reason as downloadFile). */
export async function fetchSecurityPolicy(): Promise<string> {
  const base = (import.meta.env.VITE_API_URL ?? '/api/v1').replace(/\/+$/, '');
  const res = await fetch(`${base}/security/policy`, {
    headers: { Accept: 'text/markdown' },
    credentials: 'same-origin',
  });
  if (!res.ok) {
    let body: unknown = null;
    try {
      body = await res.json();
    } catch {
      /* markdown servers answer text errors */
    }
    throw new ApiError(
      parseErrorEnvelope(body) ?? {
        type: 'error',
        error: 'INTERNAL_ERROR',
        message: `HTTP ${res.status}`,
        status: res.status,
      },
    );
  }
  return res.text();
}

export interface DisclosureInput {
  title: string;
  affectedComponents: string[];
  reproduction: string;
  reporterHandle: string;
  contactEmail: string;
  suggestedSeverity: string;
  attributionRequested: boolean;
}

/** POST /security/disclosures — 201 {report_id, status, duplicate}. */
export async function submitDisclosure(
  input: DisclosureInput,
  api: Api = apiClient,
): Promise<{ reportId?: string; status?: string; duplicate: boolean }> {
  const res = await api.post<unknown>('/security/disclosures', {
    title: input.title,
    affected_components: input.affectedComponents,
    reproduction: input.reproduction,
    reporter_handle: input.reporterHandle,
    contact_email: input.contactEmail,
    suggested_severity: input.suggestedSeverity,
    attribution_requested: input.attributionRequested,
    website: '', // honeypot field — humans leave it empty
  });
  const r = isRecord(res) ? res : {};
  return {
    reportId: str(r['report_id']),
    status: str(r['status']),
    duplicate: bool(r['duplicate']) ?? false,
  };
}
