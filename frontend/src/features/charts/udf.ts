/**
 * UDF-compatible history adapter for the chart surface (Task 10.3.4).
 *
 * The backend serves pre-materialized klines (services/internal/marketapi
 * → fx_klines, spec §10.3); this module maps the UDF verbs onto that REST
 * surface so the chart layer is interchangeable with a standard UDF
 * datafeed:
 *
 *   resolveSymbol(symbol)              → SymbolInfo (forex, 24x5 session)
 *   getBars(sym, resolution, from, to) → GET /klines/{symbol}?interval=…
 *   resolution → canonical interval    → KLINE_INTERVALS (§24 #264)
 *
 * Time is UTCTimestamp (whole seconds) — Lightweight Charts' native
 * horizontal scale.
 */
import type { ApiClient } from '@/lib/api';
import { fetchKlines } from '@/lib/market/api';
import { KLINE_INTERVAL_SET, KLINE_INTERVAL_MS, type KlineInterval } from '@/lib/market/channels';
import type { Kline } from '@/lib/market/wire';

/** UDF bar — `time` is seconds (UTCTimestamp). */
export interface UdfBar {
  time: number;
  open: number;
  high: number;
  low: number;
  close: number;
  volume: number;
}

export interface UdfSymbolInfo {
  name: string;
  ticker: string;
  type: 'forex';
  /** UDF session string: 24h, Mon–Fri (FX trading week, spec §6.1 —
   * Sunday 21:00 UTC → Friday 22:00 UTC; the 24x5 shorthand is used in
   * practice for FX feeds). */
  session: '24x5';
  timezone: 'Etc/UTC';
  minmov: number;
  pricescale: number;
  has_seconds: boolean;
  has_intraday: boolean;
  has_daily: boolean;
  has_weekly_and_monthly: boolean;
  supported_resolutions: readonly string[];
  currency_code?: string;
}

/** Map a UDF resolution token to the canonical kline interval, or null
 * when the resolution isn't in the supported 13-timeframe set. UDF
 * intraday resolutions are minute counts ('1','5','15','30'), hours are
 * multiples of 60 ('60','120','240','360','480'), plus '1S' seconds and
 * 'D'|'W'|'M' calendar frames. */
export function resolutionToInterval(resolution: string): KlineInterval | null {
  const map: Record<string, KlineInterval> = {
    '1S': '1s',
    '1': '1m',
    '5': '5m',
    '15': '15m',
    '30': '30m',
    '60': '1h',
    '120': '2h',
    '240': '4h',
    '360': '6h',
    '480': '8h',
    D: '1D',
    '1D': '1D',
    W: '1W',
    '1W': '1W',
    M: '1M',
    '1M': '1M',
  };
  return map[resolution] ?? null;
}

/** UDF resolution token for a canonical interval (inverse of the above). */
export function intervalToResolution(interval: KlineInterval): string {
  const map: Record<KlineInterval, string> = {
    '1s': '1S',
    '1m': '1',
    '5m': '5',
    '15m': '15',
    '30m': '30',
    '1h': '60',
    '2h': '120',
    '4h': '240',
    '6h': '360',
    '8h': '480',
    '1D': 'D',
    '1W': 'W',
    '1M': 'M',
  };
  return map[interval];
}

export function resolveSymbol(symbol: string, opts?: { pricescale?: number }): UdfSymbolInfo {
  return {
    name: symbol,
    ticker: symbol,
    type: 'forex',
    session: '24x5',
    timezone: 'Etc/UTC',
    minmov: 1,
    pricescale: opts?.pricescale ?? 100000, // 5dp default, majors
    has_seconds: true,
    has_intraday: true,
    has_daily: true,
    has_weekly_and_monthly: true,
    supported_resolutions: [
      '1S',
      '1',
      '5',
      '15',
      '30',
      '60',
      '120',
      '240',
      '360',
      '480',
      'D',
      'W',
      'M',
    ],
  };
}

/** REST kline row → UDF bar (seconds + numeric OHLCV for the chart API). */
export function klineToBar(k: Kline): UdfBar {
  return {
    time: Math.floor(k.openTimeMs / 1000),
    open: Number(k.open),
    high: Number(k.high),
    low: Number(k.low),
    close: Number(k.close),
    volume: Number(k.volume),
  };
}

/** getBars — fetch materialized klines, ascending time order, deduped by
 * open_time (the API may overlap pages). Returns [] for "no data" per
 * UDF convention (callers distinguish via response.count). */
export async function getBars(
  api: ApiClient,
  symbol: string,
  interval: KlineInterval,
  opts: { fromMs?: number; toMs?: number; limit?: number } = {},
): Promise<UdfBar[]> {
  const res = await fetchKlines(api, {
    symbol,
    interval,
    limit: opts.limit ?? 500,
    fromMs: opts.fromMs,
    toMs: opts.toMs,
  });
  const seen = new Set<number>();
  const bars: UdfBar[] = [];
  for (const k of res.data) {
    const b = klineToBar(k);
    if (!seen.has(b.time)) {
      seen.add(b.time);
      bars.push(b);
    }
  }
  bars.sort((a, b) => a.time - b.time);
  return bars;
}

/** Merge a live kline WS frame into the bar series expects: returns the
 * bar to feed `series.update`. The update lands on the currently-open
 * bar while `closed=false`, then rolls to the next open_time. Strictly
 * a pure transform — chart-side dedup is lwc's job. */
export function liveKlineToBar(k: Kline): UdfBar {
  return klineToBar(k);
}

/** Fixed-width bucket size for an interval, or null for calendar frames
 * (1W/1M don't have a constant ms width). */
export function intervalBucketMs(interval: KlineInterval): number | null {
  return KLINE_INTERVAL_MS[interval] ?? null;
}

/** Guard used by the chart layer — interval strings coming from UI
 * controls must be canonical before hitting the WS/REST grammar. */
export function isKlineInterval(s: string): s is KlineInterval {
  return KLINE_INTERVAL_SET.has(s);
}
