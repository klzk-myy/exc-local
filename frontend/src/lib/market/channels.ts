/**
 * WS channel grammar builders — mirrors `services/internal/marketdata/
 * channels.go` (the market-data contract):
 *
 *   {type}@{target}[:{params}]   public typed channels
 *   private:{name}               auth-gated channels
 *
 * Canonical forms:
 *   depth@EUR/USD            → default variant 20:100
 *   depth@EUR/USD:5:100      → 5 levels @100ms cadence
 *   kline@EUR/USD_1h         → timeframe suffix on the target
 *   trades@EUR/USD, bbo@EUR/USD, ticker@EUR/USD
 *   private:orders | private:positions | private:balances | private:executions
 */

/** Depth subscription levels/cadences — §24 #265 / Task 6.3.15 supported
 * sets. Anything else binds a dead subscription (server rejects with
 * INVALID_DEPTH_LIMIT), so the builders only emit this cross product. */
export const DEPTH_LEVELS = [5, 10, 20] as const;
export type DepthLevels = (typeof DEPTH_LEVELS)[number];
export const DEPTH_CADENCES_MS = [100, 250, 1000] as const;
export type DepthCadenceMs = (typeof DEPTH_CADENCES_MS)[number];

export function depthChannel(
  symbol: string,
  levels: DepthLevels = 20,
  cadenceMs: DepthCadenceMs = 100,
): string {
  if (levels === 20 && cadenceMs === 100) return `depth@${symbol}`;
  return `depth@${symbol}:${levels}:${cadenceMs}`;
}

/** Canonical 13-timeframe kline set (spec §24 #264 / Task 6.3.14 —
 * channel grammar accepts the lowercase forms too, but the venue
 * documents the canonical casing below). */
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
export const KLINE_INTERVAL_SET: ReadonlySet<string> = new Set(KLINE_INTERVALS);

/** Interval length in ms — undefined for the calendar-boundary frames
 * (1W/1M are not fixed-width). Used for live-bar bucketing only. */
export const KLINE_INTERVAL_MS: Readonly<Record<KlineInterval, number | undefined>> = {
  '1s': 1_000,
  '1m': 60_000,
  '5m': 300_000,
  '15m': 900_000,
  '30m': 1_800_000,
  '1h': 3_600_000,
  '2h': 7_200_000,
  '4h': 14_400_000,
  '6h': 21_600_000,
  '8h': 28_800_000,
  '1D': 86_400_000,
  '1W': undefined,
  '1M': undefined,
};

export function klineChannel(symbol: string, interval: KlineInterval): string {
  return `kline@${symbol}_${interval}`;
}

export const tradesChannel = (symbol: string): string => `trades@${symbol}`;
export const aggTradesChannel = (symbol: string): string => `aggTrades@${symbol}`;
export const bboChannel = (symbol: string): string => `bbo@${symbol}`;
export const tickerChannel = (symbol: string): string => `ticker@${symbol}`;
export const bookChannel = (symbol: string): string => `book@${symbol}`;

/** §10.5 auth-gated private channel set (internal/ws/server.go). */
export const PRIVATE_CHANNELS = {
  orders: 'private:orders',
  executions: 'private:executions',
  positions: 'private:positions',
  balances: 'private:balances',
} as const;
export type PrivateChannel = (typeof PRIVATE_CHANNELS)[keyof typeof PRIVATE_CHANNELS];
