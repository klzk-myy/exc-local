/**
 * History explorer adapters (Task 10.5.3.22) — the Phase-23 historical
 * read surface plus async export jobs.
 *
 *   GET /history/{trades,ticks,klines,block-trades}/{symbol}
 *   GET /history/swap-rates?symbol=&from=&to=
 *   GET /history/trades/{symbol}/export?kind=&format=&async=1
 *   GET /export-jobs[?cursor=] · GET /export-jobs/{id}
 *
 * List responses use the §8.8 envelope {data, next_cursor, limit, total}
 * extended with {access_tier, delayed, degraded?} — the client renders
 * the tier/freshness metadata verbatim so a free-tier 15-minute-delayed
 * tape never masquerades as realtime.
 */
import { apiClient } from '@/app/runtime';
import type { ApiClient } from '@/lib/api';

type Api = Pick<ApiClient, 'get'>;

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined);
const num = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined;
const bool = (v: unknown): boolean | undefined => (typeof v === 'boolean' ? v : undefined);

/** Access/freshness metadata shared by every history envelope. */
export interface AccessMeta {
  access_tier?: string;
  delayed?: boolean;
  degraded?: boolean;
}

export interface Page<T> {
  rows: T[];
  nextCursor: string | null;
  total?: number;
  meta: AccessMeta;
}

function parsePage<T>(v: unknown, row: (r: unknown) => T | null): Page<T> {
  const r = isRecord(v) ? v : {};
  const data = Array.isArray(r['data']) ? r['data'] : [];
  return {
    rows: data.map(row).filter((x): x is T => x !== null),
    nextCursor: str(r['next_cursor']) ?? null,
    total: num(r['total']),
    meta: {
      access_tier: str(r['access_tier']),
      delayed: bool(r['delayed']),
      degraded: bool(r['degraded']),
    },
  };
}

export interface HistQuery {
  symbol: string;
  from?: string; // RFC3339
  to?: string;
  limit?: number;
  cursor?: string;
}

function qs(q: Record<string, string | number | undefined>): string {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(q)) {
    if (v !== undefined && v !== '') p.set(k, String(v));
  }
  const s = p.toString();
  return s === '' ? '' : `?${s}`;
}

// ---------------------------------------------------------------------------
// Row projections — field names verbatim from the handler wire docs.
// ---------------------------------------------------------------------------

/** Ticks + trades share the public-tape prefix; trades add lineage. */
export interface TapeRow {
  trade_id?: number;
  instrument_id?: number;
  event_seq?: number;
  price?: string;
  quantity?: string;
  side?: string;
  maker_account_id?: string;
  taker_account_id?: string;
  time?: string;
  time_ms?: number;
}

function parseTape(v: unknown): TapeRow | null {
  if (!isRecord(v)) return null;
  return {
    trade_id: num(v['trade_id']),
    instrument_id: num(v['instrument_id']),
    event_seq: num(v['event_seq']),
    price: str(v['price']),
    quantity: str(v['quantity']),
    side: str(v['side']),
    maker_account_id: str(v['maker_account_id']),
    taker_account_id: str(v['taker_account_id']),
    time: str(v['time']),
    time_ms: num(v['time_ms']),
  };
}

export interface KlineRow {
  open_time_ms?: number;
  open?: string;
  high?: string;
  low?: string;
  close?: string;
  volume?: string;
  quote_volume?: string;
  trade_count?: number;
  closed?: boolean;
}

function parseKline(v: unknown): KlineRow | null {
  if (!isRecord(v)) return null;
  return {
    open_time_ms: num(v['open_time_ms']),
    open: str(v['open']),
    high: str(v['high']),
    low: str(v['low']),
    close: str(v['close']),
    volume: str(v['volume']),
    quote_volume: str(v['quote_volume']),
    trade_count: num(v['trade_count']),
    closed: bool(v['closed']),
  };
}

export interface BlockTapeRow {
  entry_id?: number;
  kind?: string; // PRINT | CORRECTION | BUST
  block_trade_id?: number;
  symbol?: string;
  price?: string;
  quantity?: string;
  notional_usd?: string;
  exec_ts?: string;
  pub_ts?: string;
  delay_ms?: number;
  venue_flags?: string[];
  bust?: boolean;
  corrected_by?: number;
  supersedes?: number;
}

function parseBlock(v: unknown): BlockTapeRow | null {
  if (!isRecord(v)) return null;
  return {
    entry_id: num(v['entry_id']),
    kind: str(v['kind']),
    block_trade_id: num(v['block_trade_id']),
    symbol: str(v['symbol']),
    price: str(v['price']),
    quantity: str(v['quantity']),
    notional_usd: str(v['notional_usd']),
    exec_ts: str(v['exec_ts']),
    pub_ts: str(v['pub_ts']),
    delay_ms: num(v['delay_ms']),
    venue_flags: Array.isArray(v['venue_flags'])
      ? (v['venue_flags'] as unknown[]).filter((x): x is string => typeof x === 'string')
      : undefined,
    bust: bool(v['bust']),
    corrected_by: num(v['corrected_by']),
    supersedes: num(v['supersedes']),
  };
}

export interface SwapRateRow {
  instrument_id?: number;
  symbol?: string;
  effective_date?: string;
  long_points?: string;
  short_points?: string;
  long_markup_bps?: string;
  short_markup_bps?: string;
  days_applied?: number;
  triple?: boolean;
  accrual_count?: number;
  source?: string;
  ingested_at?: string;
}

function parseSwap(v: unknown): SwapRateRow | null {
  if (!isRecord(v)) return null;
  return {
    instrument_id: num(v['instrument_id']),
    symbol: str(v['symbol']),
    effective_date: str(v['effective_date']),
    long_points: str(v['long_points']),
    short_points: str(v['short_points']),
    long_markup_bps: str(v['long_markup_bps']),
    short_markup_bps: str(v['short_markup_bps']),
    days_applied: num(v['days_applied']),
    triple: bool(v['triple']),
    accrual_count: num(v['accrual_count']),
    source: str(v['source']),
    ingested_at: str(v['ingested_at']),
  };
}

// ---------------------------------------------------------------------------
// Fetchers.
// ---------------------------------------------------------------------------

export function fetchTrades(q: HistQuery, api: Api = apiClient) {
  return api
    .get<unknown>(
      `/history/trades/${encodeURIComponent(q.symbol)}${qs({
        from: q.from,
        to: q.to,
        limit: q.limit,
        cursor: q.cursor,
      })}`,
    )
    .then((v) => parsePage(v, parseTape));
}

export function fetchTicks(q: HistQuery, api: Api = apiClient) {
  return api
    .get<unknown>(
      `/history/ticks/${encodeURIComponent(q.symbol)}${qs({
        from: q.from,
        to: q.to,
        limit: q.limit,
        cursor: q.cursor,
      })}`,
    )
    .then((v) => parsePage(v, parseTape));
}

export function fetchKlines(q: HistQuery & { interval: string }, api: Api = apiClient) {
  return api
    .get<unknown>(
      `/history/klines/${encodeURIComponent(q.symbol)}${qs({
        interval: q.interval,
        from: q.from,
        to: q.to,
        limit: q.limit,
        cursor: q.cursor,
      })}`,
    )
    .then((v) => parsePage(v, parseKline));
}

export function fetchBlockTrades(q: HistQuery, api: Api = apiClient) {
  return api
    .get<unknown>(
      `/history/block-trades/${encodeURIComponent(q.symbol)}${qs({
        from: q.from,
        to: q.to,
        limit: q.limit,
        cursor: q.cursor,
      })}`,
    )
    .then((v) => parsePage(v, parseBlock));
}

/** Swap rates are query-param scoped (no path symbol). */
export function fetchSwapRates(
  q: Omit<HistQuery, 'symbol'> & { symbol?: string },
  api: Api = apiClient,
) {
  return api
    .get<unknown>(
      `/history/swap-rates${qs({
        symbol: q.symbol,
        from: q.from,
        to: q.to,
        limit: q.limit,
        cursor: q.cursor,
      })}`,
    )
    .then((v) => parsePage(v, parseSwap));
}

// ---------------------------------------------------------------------------
// Export jobs — GET /history/trades/{symbol}/export?async=1 enqueues,
// GET /export-jobs lists the caller's own jobs (owner-scoped).
// ---------------------------------------------------------------------------

export interface ExportJob {
  id: number;
  kind?: string;
  symbol?: string;
  interval?: string;
  format?: string;
  status?: string; // QUEUED | RUNNING | COMPLETED | FAILED
  row_count?: number;
  truncated?: boolean;
  sha256?: string;
  download_url?: string;
  expires_at?: string;
  error?: string;
  created_at?: string;
  finished_at?: string;
}

export function parseJob(v: unknown): ExportJob | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  if (id === undefined) return null;
  return {
    id,
    kind: str(v['kind']),
    symbol: str(v['symbol']),
    interval: str(v['interval']),
    format: str(v['format']),
    status: str(v['status']),
    row_count: num(v['row_count']),
    truncated: bool(v['truncated']),
    sha256: str(v['sha256']),
    download_url: str(v['download_url']),
    expires_at: str(v['expires_at']),
    error: str(v['error']),
    created_at: str(v['created_at']),
    finished_at: str(v['finished_at']),
  };
}

/** Enqueue an async export. 202 → {job, status_url}; the file itself is
 * fetched later through /export-jobs/{id}/download. */
export async function enqueueExport(
  args: {
    symbol: string;
    kind: 'trades' | 'ticks' | 'klines';
    format: 'csv' | 'json' | 'parquet';
    interval?: string;
    from?: string;
    to?: string;
    limit?: number;
  },
  api: Api = apiClient,
): Promise<ExportJob | null> {
  const res = await api.get<unknown>(
    `/history/trades/${encodeURIComponent(args.symbol)}/export${qs({
      kind: args.kind,
      format: args.format,
      interval: args.interval,
      from: args.from,
      to: args.to,
      limit: args.limit,
      async: '1',
    })}`,
  );
  return parseJob(isRecord(res) ? res['job'] : res);
}

export function fetchExportJobs(cursor?: string, api: Api = apiClient) {
  return api
    .get<unknown>(`/export-jobs${qs({ cursor, limit: 50 })}`)
    .then((v) => parsePage(v, parseJob));
}
