/**
 * Candle (kline) fetch adapter — Task 10.3.16 data seam.
 *
 * Consumes `GET /api/v1/klines/{symbol}?interval=&from=&to=&limit=`
 * (services/internal/api/handlers_market.go MarketKlines). The endpoint
 * returns pre-materialized OHLCV bars from `fx_klines` ordered by
 * open_time DESC, a `next_cursor` (earliest returned open_time, epoch
 * millis as a string) when the page is full — page backwards with
 * `to=<next_cursor>`.
 *
 * The response's decimal-string prices are converted to numbers here at
 * the adapter boundary (spec §5.3 money-as-strings contract); callers
 * get a clean Candle[] in ASCENDING time order.
 */
import type { ApiClient } from '@/lib/api/client';

export interface Candle {
  openTimeMs: number;
  open: number;
  high: number;
  low: number;
  close: number;
  volume: number;
  closed: boolean;
}

/** Canonical interval set (marketapi.KlineIntervals). */
export const KLINE_INTERVALS = [
  '1s',
  '1m',
  '5m',
  '15m',
  '30m',
  '1h',
  '2h',
  '4h',
  '6h',
  '8h',
  '1D',
  '1W',
  '1M',
] as const;
export type KlineInterval = (typeof KLINE_INTERVALS)[number];

interface KlineWire {
  open_time_ms: number;
  open: string;
  high: string;
  low: string;
  close: string;
  volume: string;
  quote_volume?: string;
  trade_count?: number;
  closed?: boolean;
}

interface KlineResponse {
  symbol: string;
  interval: string;
  data: KlineWire[];
  count: number;
  limit: number;
  next_cursor?: string;
}

function toCandle(k: KlineWire): Candle {
  return {
    openTimeMs: k.open_time_ms,
    open: Number(k.open),
    high: Number(k.high),
    low: Number(k.low),
    close: Number(k.close),
    volume: Number(k.volume),
    closed: k.closed ?? false,
  };
}

export interface FetchKlinesOptions {
  interval?: KlineInterval | (string & {});
  /** Bounds on open_time (epoch millis). */
  fromMs?: number;
  toMs?: number;
  /** Page size (server clamps to [1,1500], default 500). */
  limit?: number;
  /** Safety cap on backwards paging. */
  maxPages?: number;
  signal?: AbortSignal;
}

/**
 * Fetch candles covering [fromMs, toMs] by walking the `next_cursor`
 * backwards. Returns ascending time order. Throws ApiError on failure —
 * NOT_FOUND for unknown symbols, INVALID_REQUEST for bad intervals.
 */
export async function fetchKlines(
  api: ApiClient,
  symbol: string,
  opts: FetchKlinesOptions = {},
): Promise<Candle[]> {
  const interval = opts.interval ?? '1h';
  const limit = opts.limit ?? 1500;
  const maxPages = opts.maxPages ?? 10;
  let to = opts.toMs;
  const collected: Candle[] = [];

  for (let page = 0; page < maxPages; page++) {
    const res = await api.get<KlineResponse>(`/klines/${encodeURIComponent(symbol)}`, {
      query: {
        interval,
        limit,
        from: opts.fromMs,
        to,
      },
      signal: opts.signal,
    });
    const batch = res.data.map(toCandle);
    if (batch.length === 0) break;
    // Server returns DESC (newest first); prepend so the final array is ASC.
    collected.unshift(...batch);
    const cursor = res.next_cursor;
    if (!cursor) break;
    const nextTo = Number(cursor);
    if (!Number.isFinite(nextTo) || nextTo <= 0) break;
    if (opts.fromMs !== undefined && nextTo <= opts.fromMs) break;
    to = nextTo;
  }
  collected.sort((a, b) => a.openTimeMs - b.openTimeMs);
  return collected;
}
