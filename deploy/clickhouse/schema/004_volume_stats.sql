-- 004_volume_stats.sql — trading volume & order-counter rollups
-- (spec §16.2, Tasks 20.3.1/20.3.5).
--
-- CONTRACT (read side: services/internal/analytics/stats.go): columns are
-- (symbol, bucket_start, granularity, volume, quote_volume, trade_count,
-- orders_submitted, orders_filled) — SummingMergeTree on
-- (symbol, bucket_start, granularity). Every insert is a DELTA row:
-- counters accumulate by addition on merge, so writers append deltas and
-- never rewrite buckets.
--
-- Two row grains share the table:
--   '1h' — written by volume_stats_hourly_mv over `ticks` below; carries
--          volume/quote_volume/trade_count (order counters 0). This is
--          the Task 20.3.1 "batch aggregation" criterion — no daily batch
--          job needed; "per day" volume is a toStartOfDay GROUP BY over
--          the '1h' rows at read time.
--   '1d' — counters-only rows (volume columns 0) carrying
--          orders_submitted/orders_filled deltas, written by
--          VolumeStatsStore.SyncOrdersCount and the ETL consumer's
--          OrderNew/TradeFill counting. FillRates() reads these.
-- The granularity discriminator is what keeps the axes honest: Volume()
-- filters '1h', FillRates() filters '1d' — neither double-counts.
--
-- Retention: 5-year TTL (§16.2/§19.12).

CREATE TABLE IF NOT EXISTS exchange_analytics.volume_stats
(
    symbol           LowCardinality(String),
    bucket_start     DateTime('UTC'),
    granularity      LowCardinality(String),  -- '1h' | '1d'
    volume           Decimal(38, 8),          -- sum(quantity)
    quote_volume     Decimal(38, 8),          -- sum(price*quantity) @ scale 8
    trade_count      UInt64,
    orders_submitted UInt64,
    orders_filled    UInt64
)
ENGINE = SummingMergeTree()
PARTITION BY toYYYYMM(bucket_start)
ORDER BY (symbol, bucket_start, granularity)
TTL bucket_start + INTERVAL 5 YEAR;

CREATE MATERIALIZED VIEW IF NOT EXISTS exchange_analytics.volume_stats_hourly_mv
TO exchange_analytics.volume_stats AS
SELECT
    symbol,
    toStartOfHour(ts) AS bucket_start,
    '1h' AS granularity,
    sum(quantity) AS volume,
    -- price*quantity promotes to Decimal(76,16); round back to scale 8 so
    -- the projection matches the target column type exactly.
    sum(toDecimal128(price * quantity, 8)) AS quote_volume,
    count() AS trade_count,
    toUInt64(0) AS orders_submitted,
    toUInt64(0) AS orders_filled
FROM exchange_analytics.ticks
GROUP BY symbol, bucket_start;
