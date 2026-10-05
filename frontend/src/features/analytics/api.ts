/**
 * Market analytics & intelligence fetchers (Task 10.5.3.21).
 *
 * Route contracts (routes_v1.go + handlers_market_stats.go /
 * handlers_analytics.go / handlers_stats.go / handlers_l3.go):
 *   GET /stats/24h[/{symbol}]               {window,count,data[],server_time_ms}
 *   GET /analytics/open-interest/{symbol}?interval=1h|4h|1d
 *         {data:{symbol,interval,current{open_interest,open_interest_notional,
 *          positions,stale,as_of_ms}|null,insufficient_data?,series[]}}
 *   GET /analytics/long-short-ratio/{symbol}?period=
 *   GET /analytics/taker-flow/{symbol}?period=
 *         {data:{symbol,period,delay_ms,delayed,as_of_ms,points[]}}
 *         — points below the 100-account cohort floor carry suppressed:true
 *           and nothing else (never silently truncated, Phase-23).
 *   GET /market/positioning?symbol=        {data:{accounts,long_accounts,
 *          short_accounts,long_notional,short_notional,gross_notional,
 *          delay_ms,delayed,as_of_ms}}
 *   GET /market/performance                {data:{status,as_of_ms,delay_ms,
 *          held_age_ms?,divergent_metrics?,venue?,pairs?}}
 *   GET /market/depth?symbol=&limit=       BookSnapshot {bids,asks,seq,…}
 *   GET /market-data/snapshot?symbol=&level=L2  same BookSnapshot
 *   GET /market-data/l3-snapshot/{symbol}  TierProfessional → 402/403 honest;
 *         {symbol,l3_seq,wal_seq,asof_ms,count,orders[],next_cursor?}
 *   GET /analytics/volume?from&to&granularity&symbol&limit
 *         {from,to,generated_at,count,limit,truncated,rows[]}
 *   GET /analytics/stats?from&to           {fill_rates[],tiers[],totals…}
 *   GET /analytics/pnl?from&to             auth-only {rows:[pnlRowJSON]}
 */
import { apiClient } from '@/app/runtime';
import type { ApiClient } from '@/lib/api';

type Api = Pick<ApiClient, 'get'>;

// ---- rolling 24h stats -----------------------------------------------------

export interface Stats24h {
  symbol: string;
  open?: string;
  high?: string;
  low?: string;
  last?: string;
  price_change?: string;
  price_change_pct?: string;
  volume: string;
  quote_volume: string;
  trade_count: number;
}

export async function fetchStats24h(
  symbol?: string,
  api: Api = apiClient,
): Promise<{ rows: Stats24h[]; serverTimeMs?: number }> {
  if (symbol !== undefined && symbol !== '') {
    const row = await api.get<Stats24h>(`/stats/24h/${encodeURIComponent(symbol)}`);
    return { rows: [row] };
  }
  const res = await api.get<{ data?: Stats24h[]; server_time_ms?: number }>('/stats/24h');
  return { rows: res.data ?? [], serverTimeMs: res.server_time_ms };
}

// ---- sentiment series -------------------------------------------------------

export interface OiCurrent {
  open_interest?: string;
  open_interest_notional?: string;
  positions?: number;
  stale?: boolean;
  as_of_ms?: number;
}
export interface OiSeriesPoint {
  bucket_start_ms?: number;
  open_interest?: string;
  [k: string]: unknown;
}
export interface OiData {
  symbol?: string;
  interval?: string;
  current?: OiCurrent | null;
  insufficient_data?: boolean;
  series?: OiSeriesPoint[];
}
export async function fetchOpenInterest(
  symbol: string,
  interval: string,
  api: Api = apiClient,
): Promise<OiData> {
  const res = await api.get<{ data?: OiData }>(
    `/analytics/open-interest/${encodeURIComponent(symbol)}?interval=${encodeURIComponent(interval)}`,
  );
  return res.data ?? {};
}

export interface LongShortPoint {
  bucket_start_ms?: number;
  open?: boolean;
  suppressed?: boolean;
  accounts?: number;
  long_ratio?: string;
  short_ratio?: string;
  long_short_ratio?: string;
}
export interface TakerFlowPoint {
  bucket_start_ms?: number;
  suppressed?: boolean;
  trades?: number;
  buy_volume?: string;
  sell_volume?: string;
  buy_notional?: string;
  sell_notional?: string;
  buy_sell_ratio?: string;
}
export interface SeriesData<T> {
  symbol?: string;
  period?: string;
  delay_ms?: number;
  delayed?: boolean;
  as_of_ms?: number;
  insufficient_data?: boolean;
  points?: T[];
}
export async function fetchLongShort(
  symbol: string,
  period: string,
  api: Api = apiClient,
): Promise<SeriesData<LongShortPoint>> {
  const res = await api.get<{ data?: SeriesData<LongShortPoint> }>(
    `/analytics/long-short-ratio/${encodeURIComponent(symbol)}?period=${encodeURIComponent(period)}`,
  );
  return res.data ?? {};
}
export async function fetchTakerFlow(
  symbol: string,
  period: string,
  api: Api = apiClient,
): Promise<SeriesData<TakerFlowPoint>> {
  const res = await api.get<{ data?: SeriesData<TakerFlowPoint> }>(
    `/analytics/taker-flow/${encodeURIComponent(symbol)}?period=${encodeURIComponent(period)}`,
  );
  return res.data ?? {};
}

// ---- venue-level market intel ----------------------------------------------

export interface Positioning {
  symbol?: string;
  as_of_ms?: number;
  delay_ms?: number;
  delayed?: boolean;
  insufficient_data?: boolean;
  accounts?: number;
  long_accounts?: number;
  short_accounts?: number;
  long_notional?: string;
  short_notional?: string;
  gross_notional?: string;
}
export async function fetchPositioning(symbol: string, api: Api = apiClient): Promise<Positioning> {
  const res = await api.get<{ data?: Positioning }>(
    `/market/positioning?symbol=${encodeURIComponent(symbol)}`,
  );
  return res.data ?? {};
}

/** GET /market/taker-volume?symbol= — same points shape as
 * /analytics/taker-flow but symbol arrives as a query param. */
export async function fetchMarketTakerVolume(
  symbol: string,
  api: Api = apiClient,
): Promise<SeriesData<TakerFlowPoint>> {
  const res = await api.get<{ data?: SeriesData<TakerFlowPoint> }>(
    `/market/taker-volume?symbol=${encodeURIComponent(symbol)}`,
  );
  return res.data ?? {};
}

export interface VenuePerformance {
  status?: string;
  as_of_ms?: number;
  delay_ms?: number;
  held_age_ms?: number;
  divergent_metrics?: string[];
  venue?: {
    median_exec_latency_ms?: string | null;
    fill_rate_24h?: string | null;
    fill_rate_7d?: string | null;
    uptime_24h?: number | null;
    uptime_30d?: number | null;
  };
  pairs?: { symbol?: string; insufficient_data?: boolean }[];
}
export async function fetchPerformance(api: Api = apiClient): Promise<VenuePerformance> {
  const res = await api.get<{ data?: VenuePerformance }>('/market/performance');
  return res.data ?? {};
}

// ---- book snapshots ---------------------------------------------------------

export interface BookLevel {
  price?: string;
  quantity?: string;
  [k: string]: unknown;
}
export interface BookSnapshot {
  symbol?: string;
  seq?: number;
  depth?: number;
  bids?: BookLevel[];
  asks?: BookLevel[];
  updated_at_ms?: number;
}
export function fetchDepth(symbol: string, limit = 20, api: Api = apiClient) {
  return api.get<BookSnapshot>(`/market/depth?symbol=${encodeURIComponent(symbol)}&limit=${limit}`);
}

export interface L3Snapshot {
  symbol?: string;
  l3_seq?: number;
  wal_seq?: number;
  asof_ms?: number;
  count?: number;
  fresh?: boolean;
  truncated?: boolean;
  orders?: Record<string, unknown>[];
  next_cursor?: string | null;
}
export function fetchL3(symbol: string, api: Api = apiClient) {
  return api.get<L3Snapshot>(`/market-data/l3-snapshot/${encodeURIComponent(symbol)}`);
}

// ---- venue volume / stats / account pnl -------------------------------------

export interface VolumeRow {
  symbol?: string;
  bucket_start?: string;
  granularity?: string;
  volume?: string;
  quote_volume?: string;
  trade_count?: number;
}
export async function fetchVolume(
  q: { symbol?: string; granularity?: '1h' | '1d'; limit?: number },
  api: Api = apiClient,
): Promise<{ rows: VolumeRow[]; truncated?: boolean; generated_at?: string }> {
  const p = new URLSearchParams();
  if (q.symbol !== undefined && q.symbol !== '') p.set('symbol', q.symbol);
  p.set('granularity', q.granularity ?? '1h');
  p.set('limit', String(q.limit ?? 50));
  const res = await api.get<{
    rows?: VolumeRow[];
    truncated?: boolean;
    generated_at?: string;
  }>(`/analytics/volume?${p.toString()}`);
  return { rows: res.rows ?? [], truncated: res.truncated, generated_at: res.generated_at };
}

export interface VenueStats {
  fill_rates?: {
    symbol?: string;
    orders_submitted?: number;
    orders_filled?: number;
    fill_rate?: string | null;
  }[];
  tiers?: { symbol?: string; tier?: string; trade_count?: number; volume?: string }[];
  totals?: Record<string, unknown>;
}
export async function fetchVenueStats(api: Api = apiClient): Promise<VenueStats> {
  const res = await api.get<Record<string, unknown>>('/analytics/stats');
  return {
    fill_rates: Array.isArray(res['fill_rates'])
      ? (res['fill_rates'] as VenueStats['fill_rates'])
      : Array.isArray(res['fills'])
        ? (res['fills'] as VenueStats['fill_rates'])
        : [],
    tiers: Array.isArray(res['tiers']) ? (res['tiers'] as VenueStats['tiers']) : [],
    totals:
      typeof res['totals'] === 'object' && res['totals'] !== null
        ? (res['totals'] as Record<string, unknown>)
        : undefined,
  };
}

export interface PnlRow {
  account_id?: number;
  instrument_id?: number;
  symbol?: string;
  day?: string;
  realized?: string;
  unrealized?: string;
  fees?: string;
  net?: string;
}
export async function fetchAccountPnl(api: Api = apiClient): Promise<PnlRow[]> {
  const res = await api.get<Record<string, unknown>>('/analytics/pnl');
  const rows = res['rows'] ?? res['data'];
  return Array.isArray(rows) ? (rows as PnlRow[]) : [];
}
