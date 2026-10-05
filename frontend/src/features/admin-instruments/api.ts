/**
 * admin-instruments wire seam (Phase-10.5 Task 10.5.3.11) — instrument
 * lifecycle, listing governance, market schedule, and the
 * dual-controlled margin/leverage policy cells.
 *
 *   (instrument list + lifecycle verbs live in features/admin/
 *    InstrumentsPanel — not duplicated here)
 *   GET/POST        /api/v1/admin/listing-proposals[?status=]
 *   POST            /api/v1/admin/listing-proposals/{id}/review  APPROVE = DC
 *   GET/PUT         /api/v1/admin/instruments/{symbol}/auction-calendar
 *   GET/POST/PUT/DEL /api/v1/admin/market-schedule[/overrides[/{id}]]
 *   POST            /api/v1/admin/margin-param-changes           202 + §13.12 gate
 *   GET/POST        /api/v1/admin/entity-leverage-policy         matrix + DC cell
 *
 * Dual-control surfaces respond 202 {status:"PENDING", dual_control_id,
 * required_approver, expires_at} — rendered as queued, never applied.
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
// Dual-control pending response (202).
// ---------------------------------------------------------------------------

export interface PendingDual {
  dualControlId: number;
  operation: string;
  requiredApprover: string;
  expiresAt?: number;
}

/** Parses a 202 writePendingDual body — throws if the shape is wrong. */
export function parsePending(raw: unknown): PendingDual {
  if (!isRecord(raw)) throw malformed('dual-control pending');
  return {
    dualControlId: num(raw['dual_control_id']) ?? 0,
    operation: str(raw['operation']) ?? '',
    requiredApprover: str(raw['required_approver']) ?? '',
    expiresAt: num(raw['expires_at']),
  };
}

// The instrument list + lifecycle verbs (activate/restrict/cancel-only/
// suspend/halt/resume/delist + create/edit) live in
// `@/features/admin/InstrumentsPanel` (Task 15.3.2 surface) — this
// feature adds only the surfaces the plan still lacks: listing
// proposals, auction calendar, market schedule, margin/leverage policy.
// ---------------------------------------------------------------------------
// Listing proposals.
// ---------------------------------------------------------------------------

export interface ListingProposal {
  id: number;
  symbol: string;
  status: string;
  reason: string;
  reviewedBy?: number;
  instrumentId?: number;
  activateAt?: string;
  autoChecks?: { fails: string[] };
  createdAt: string;
}

export async function fetchListingProposals(
  api: BoundAdminApi,
  status?: string,
): Promise<ListingProposal[]> {
  const raw = await api.get<unknown>(
    `/admin/listing-proposals${status !== undefined && status !== '' ? `?status=${status}` : ''}`,
  );
  if (!isRecord(raw)) throw malformed('proposals');
  return arr(raw['proposals']).flatMap((p) => {
    if (!isRecord(p)) return [];
    const ac = isRecord(p['auto_checks']) ? p['auto_checks'] : undefined;
    return [
      {
        id: num(p['id']) ?? 0,
        symbol: str(p['symbol']) ?? '',
        status: str(p['status']) ?? '',
        reason: str(p['reason']) ?? '',
        reviewedBy: num(p['reviewed_by']),
        instrumentId: num(p['instrument_id']),
        activateAt: str(p['activate_at']),
        autoChecks:
          ac !== undefined
            ? {
                fails: arr(ac['reference_fails']).flatMap((f) =>
                  typeof f === 'string' ? [f] : [],
                ),
              }
            : undefined,
        createdAt: str(p['created_at']) ?? '',
      },
    ];
  });
}

export async function createListingProposal(
  api: BoundAdminApi,
  input: {
    symbol: string;
    reference: Record<string, unknown>;
    oracleFeeds: string[];
    riskDefaults: Record<string, unknown>;
    activateAt?: string;
    reason: string;
  },
): Promise<void> {
  await api.post('/admin/listing-proposals', {
    symbol: input.symbol,
    reference: input.reference,
    oracle_feeds: input.oracleFeeds,
    risk_defaults: input.riskDefaults,
    activate_at: input.activateAt,
    reason: input.reason,
  });
}

/** APPROVE files the four-eyes request (202); REVIEW/REJECT apply (200). */
export async function reviewListingProposal(
  api: BoundAdminApi,
  id: number,
  body: { action: 'REVIEW' | 'APPROVE' | 'REJECT'; note: string; activateAt?: string },
): Promise<'applied' | PendingDual> {
  const raw = await api.post<unknown>(`/admin/listing-proposals/${id}/review`, {
    action: body.action,
    note: body.note,
    activate_at: body.activateAt,
  });
  if (isRecord(raw) && str(raw['status']) === 'PENDING') {
    return parsePending(raw);
  }
  return 'applied';
}

// ---------------------------------------------------------------------------
// Auction calendar (per symbol; PUT is a full-replace dual-control op).
// ---------------------------------------------------------------------------

export interface CalendarEntry {
  id?: number;
  auctionType: string;
  benchmark?: string;
  triggerTime: string;
  timezone: string;
  recurrence: string;
  enabled: boolean;
  lastFiredAt?: string;
}

export async function fetchAuctionCalendar(
  api: BoundAdminApi,
  symbol: string,
): Promise<CalendarEntry[]> {
  const raw = await api.get<unknown>(
    `/admin/instruments/${encodeURIComponent(symbol)}/auction-calendar`,
  );
  if (!isRecord(raw)) throw malformed('calendar');
  return arr(raw['entries']).flatMap((e) => {
    if (!isRecord(e)) return [];
    return [
      {
        id: num(e['id']),
        auctionType: str(e['auction_type']) ?? '',
        benchmark: str(e['benchmark']),
        triggerTime: str(e['trigger_time']) ?? '',
        timezone: str(e['timezone']) ?? '',
        recurrence: str(e['recurrence']) ?? '',
        enabled: bool(e['enabled']),
        lastFiredAt: str(e['last_fired_at']),
      },
    ];
  });
}

export async function putAuctionCalendar(
  api: ApiClient,
  env: AdminEnv,
  symbol: string,
  entries: CalendarEntry[],
  reason: string,
): Promise<PendingDual> {
  const raw = await api.put<unknown>(
    `/admin/instruments/${encodeURIComponent(symbol)}/auction-calendar`,
    {
      entries: entries.map((e) => ({
        auction_type: e.auctionType,
        benchmark: e.benchmark,
        trigger_time: e.triggerTime,
        timezone: e.timezone,
        recurrence: e.recurrence,
        enabled: e.enabled,
      })),
      reason,
    },
    envOpts(env),
  );
  return parsePending(raw);
}

// ---------------------------------------------------------------------------
// Market schedule + overrides.
// ---------------------------------------------------------------------------

export interface ScheduleOverride {
  id: number;
  date: string;
  closed: boolean;
  open?: string;
  close?: string;
  reason: string;
}

export interface MarketSchedule {
  openUtc: string;
  closeUtc: string;
  preOpenUtc: string;
  version: number;
  publishedAt: string;
  overrides: ScheduleOverride[];
}

function parseOverride(v: unknown): ScheduleOverride {
  if (!isRecord(v)) throw malformed('override');
  return {
    id: num(v['id']) ?? 0,
    date: str(v['date']) ?? '',
    closed: bool(v['closed']),
    open: str(v['open']),
    close: str(v['close']),
    reason: str(v['reason']) ?? '',
  };
}

export async function fetchMarketSchedule(api: BoundAdminApi): Promise<MarketSchedule> {
  const raw = await api.get<unknown>('/admin/market-schedule');
  if (!isRecord(raw)) throw malformed('market schedule');
  return {
    openUtc: str(raw['open_utc']) ?? '',
    closeUtc: str(raw['close_utc']) ?? '',
    preOpenUtc: str(raw['pre_open_utc']) ?? '',
    version: num(raw['version']) ?? 0,
    publishedAt: str(raw['published_at']) ?? '',
    overrides: arr(raw['overrides']).map(parseOverride),
  };
}

/** GET /admin/market-schedule/overrides — dedicated overrides register
 * (includes expired rows the base schedule document trims). */
export async function fetchScheduleOverrides(api: BoundAdminApi) {
  const raw = await api.get<unknown>('/admin/market-schedule/overrides');
  if (!isRecord(raw)) throw malformed('schedule overrides');
  return arr(raw['overrides'] ?? raw['items']).map(parseOverride);
}

export interface OverrideInput {
  date: string;
  closed: boolean;
  open?: string;
  close?: string;
  reason: string;
}

export async function createScheduleOverride(
  api: BoundAdminApi,
  input: OverrideInput,
): Promise<void> {
  await api.post('/admin/market-schedule/overrides', {
    date: input.date,
    closed: input.closed,
    open: input.open,
    close: input.close,
    reason: input.reason,
  });
}

export async function updateScheduleOverride(
  api: ApiClient,
  env: AdminEnv,
  id: number,
  input: OverrideInput,
): Promise<void> {
  await api.put(
    `/admin/market-schedule/overrides/${id}`,
    {
      date: input.date,
      closed: input.closed,
      open: input.open,
      close: input.close,
      reason: input.reason,
    },
    envOpts(env),
  );
}

export async function deleteScheduleOverride(
  api: ApiClient,
  env: AdminEnv,
  id: number,
): Promise<void> {
  await api.delete(`/admin/market-schedule/overrides/${id}`, envOpts(env));
}

// ---------------------------------------------------------------------------
// Margin-parameter changes (§13.12 gate) + entity leverage matrix (§13.14).
// ---------------------------------------------------------------------------

export async function submitMarginParamChange(
  api: BoundAdminApi,
  input: { parameter: string; proposedValue: unknown; runId: number; reason: string },
): Promise<PendingDual> {
  const raw = await api.post<unknown>('/admin/margin-param-changes', {
    parameter: input.parameter,
    proposed_value: input.proposedValue,
    run_id: input.runId,
    reason: input.reason,
  });
  return parsePending(raw);
}

export interface LeveragePolicyRow {
  entityCode: string;
  clientCategory: string;
  instrumentGroup: string;
  maxLeverage: number;
  effectiveFrom: string;
}

export async function fetchLeveragePolicies(api: BoundAdminApi): Promise<LeveragePolicyRow[]> {
  const raw = await api.get<unknown>('/admin/entity-leverage-policy');
  if (!isRecord(raw)) throw malformed('leverage policies');
  return arr(raw['policies']).flatMap((p) => {
    if (!isRecord(p)) return [];
    return [
      {
        entityCode: str(p['entity_code']) ?? '',
        clientCategory: str(p['client_category']) ?? '',
        instrumentGroup: str(p['instrument_group']) ?? '',
        maxLeverage: num(p['max_leverage']) ?? 0,
        effectiveFrom: str(p['effective_from']) ?? '',
      },
    ];
  });
}

export async function submitLeveragePolicy(
  api: BoundAdminApi,
  input: {
    entityCode: string;
    clientCategory: string;
    instrumentGroup: string;
    maxLeverage: number;
    effectiveFrom: string;
    reason: string;
  },
): Promise<PendingDual> {
  const raw = await api.post<unknown>('/admin/entity-leverage-policy', {
    entity_code: input.entityCode,
    client_category: input.clientCategory,
    instrument_group: input.instrumentGroup,
    max_leverage: input.maxLeverage,
    effective_from: input.effectiveFrom,
    reason: input.reason,
  });
  return parsePending(raw);
}
