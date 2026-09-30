-- 003_ohlcv.sql — the 12 persisted candle intervals (spec §16.2).
--
-- DECISION (spec-conformant): twelve SummingMergeTree tables, one per
-- persisted interval — §16.2 mandates `ohlcv_{interval}` SummingMergeTree
-- tables and Phase-06 Task 6.3.8's engine computes the candles; these
-- tables are the long-term mirror. The single-table alternative
-- (one `ohlcv` table with an `interval` column) was rejected: §16.2's
-- table list is canonical. A convenience VIEW `ohlcv` (UNION ALL with an
-- `interval` literal) is provided for queries that want one surface.
--
-- Merge semantics: SummingMergeTree sums plain numeric columns on merge,
-- which is correct for volume/quote_volume/trade_count partial rows but
-- wrong for OHLC prices — so OHLC columns are SimpleAggregateFunction
-- (max/min/anyLast). Phase-06 writes whole-bucket candle rows; a rewrite
-- of the same (symbol, bucket) replaces OHLC with the newest values and
-- sums volume counters — replays therefore never double count *and* never
-- fabricate stale prices.
--
-- Retention: 5-year TTL on every interval (§16.2, §19.12; the earlier
-- 10-year `ohlcv_1d` TTL is superseded).

CREATE TABLE IF NOT EXISTS exchange_analytics.ohlcv_1m
(
    bucket       DateTime('UTC'),
    symbol       LowCardinality(String),
    open         SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    high         SimpleAggregateFunction(max, Decimal(38, 8)),
    low          SimpleAggregateFunction(min, Decimal(38, 8)),
    close        SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    volume       Decimal(38, 8),
    quote_volume Decimal(38, 8),
    trade_count  UInt64
)
ENGINE = SummingMergeTree()
PARTITION BY toYYYYMM(bucket)
ORDER BY (symbol, bucket)
TTL bucket + INTERVAL 5 YEAR;

CREATE TABLE IF NOT EXISTS exchange_analytics.ohlcv_5m
(
    bucket       DateTime('UTC'),
    symbol       LowCardinality(String),
    open         SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    high         SimpleAggregateFunction(max, Decimal(38, 8)),
    low          SimpleAggregateFunction(min, Decimal(38, 8)),
    close        SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    volume       Decimal(38, 8),
    quote_volume Decimal(38, 8),
    trade_count  UInt64
)
ENGINE = SummingMergeTree()
PARTITION BY toYYYYMM(bucket)
ORDER BY (symbol, bucket)
TTL bucket + INTERVAL 5 YEAR;

CREATE TABLE IF NOT EXISTS exchange_analytics.ohlcv_15m
(
    bucket       DateTime('UTC'),
    symbol       LowCardinality(String),
    open         SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    high         SimpleAggregateFunction(max, Decimal(38, 8)),
    low          SimpleAggregateFunction(min, Decimal(38, 8)),
    close        SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    volume       Decimal(38, 8),
    quote_volume Decimal(38, 8),
    trade_count  UInt64
)
ENGINE = SummingMergeTree()
PARTITION BY toYYYYMM(bucket)
ORDER BY (symbol, bucket)
TTL bucket + INTERVAL 5 YEAR;

CREATE TABLE IF NOT EXISTS exchange_analytics.ohlcv_30m
(
    bucket       DateTime('UTC'),
    symbol       LowCardinality(String),
    open         SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    high         SimpleAggregateFunction(max, Decimal(38, 8)),
    low          SimpleAggregateFunction(min, Decimal(38, 8)),
    close        SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    volume       Decimal(38, 8),
    quote_volume Decimal(38, 8),
    trade_count  UInt64
)
ENGINE = SummingMergeTree()
PARTITION BY toYYYYMM(bucket)
ORDER BY (symbol, bucket)
TTL bucket + INTERVAL 5 YEAR;

CREATE TABLE IF NOT EXISTS exchange_analytics.ohlcv_1h
(
    bucket       DateTime('UTC'),
    symbol       LowCardinality(String),
    open         SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    high         SimpleAggregateFunction(max, Decimal(38, 8)),
    low          SimpleAggregateFunction(min, Decimal(38, 8)),
    close        SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    volume       Decimal(38, 8),
    quote_volume Decimal(38, 8),
    trade_count  UInt64
)
ENGINE = SummingMergeTree()
PARTITION BY toYYYYMM(bucket)
ORDER BY (symbol, bucket)
TTL bucket + INTERVAL 5 YEAR;

CREATE TABLE IF NOT EXISTS exchange_analytics.ohlcv_2h
(
    bucket       DateTime('UTC'),
    symbol       LowCardinality(String),
    open         SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    high         SimpleAggregateFunction(max, Decimal(38, 8)),
    low          SimpleAggregateFunction(min, Decimal(38, 8)),
    close        SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    volume       Decimal(38, 8),
    quote_volume Decimal(38, 8),
    trade_count  UInt64
)
ENGINE = SummingMergeTree()
PARTITION BY toYYYYMM(bucket)
ORDER BY (symbol, bucket)
TTL bucket + INTERVAL 5 YEAR;

CREATE TABLE IF NOT EXISTS exchange_analytics.ohlcv_4h
(
    bucket       DateTime('UTC'),
    symbol       LowCardinality(String),
    open         SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    high         SimpleAggregateFunction(max, Decimal(38, 8)),
    low          SimpleAggregateFunction(min, Decimal(38, 8)),
    close        SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    volume       Decimal(38, 8),
    quote_volume Decimal(38, 8),
    trade_count  UInt64
)
ENGINE = SummingMergeTree()
PARTITION BY toYYYYMM(bucket)
ORDER BY (symbol, bucket)
TTL bucket + INTERVAL 5 YEAR;

CREATE TABLE IF NOT EXISTS exchange_analytics.ohlcv_6h
(
    bucket       DateTime('UTC'),
    symbol       LowCardinality(String),
    open         SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    high         SimpleAggregateFunction(max, Decimal(38, 8)),
    low          SimpleAggregateFunction(min, Decimal(38, 8)),
    close        SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    volume       Decimal(38, 8),
    quote_volume Decimal(38, 8),
    trade_count  UInt64
)
ENGINE = SummingMergeTree()
PARTITION BY toYYYYMM(bucket)
ORDER BY (symbol, bucket)
TTL bucket + INTERVAL 5 YEAR;

CREATE TABLE IF NOT EXISTS exchange_analytics.ohlcv_8h
(
    bucket       DateTime('UTC'),
    symbol       LowCardinality(String),
    open         SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    high         SimpleAggregateFunction(max, Decimal(38, 8)),
    low          SimpleAggregateFunction(min, Decimal(38, 8)),
    close        SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    volume       Decimal(38, 8),
    quote_volume Decimal(38, 8),
    trade_count  UInt64
)
ENGINE = SummingMergeTree()
PARTITION BY toYYYYMM(bucket)
ORDER BY (symbol, bucket)
TTL bucket + INTERVAL 5 YEAR;

CREATE TABLE IF NOT EXISTS exchange_analytics.ohlcv_1d
(
    bucket       DateTime('UTC'),
    symbol       LowCardinality(String),
    open         SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    high         SimpleAggregateFunction(max, Decimal(38, 8)),
    low          SimpleAggregateFunction(min, Decimal(38, 8)),
    close        SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    volume       Decimal(38, 8),
    quote_volume Decimal(38, 8),
    trade_count  UInt64
)
ENGINE = SummingMergeTree()
PARTITION BY toYYYYMM(bucket)
ORDER BY (symbol, bucket)
TTL bucket + INTERVAL 5 YEAR;

CREATE TABLE IF NOT EXISTS exchange_analytics.ohlcv_1w
(
    bucket       DateTime('UTC'),
    symbol       LowCardinality(String),
    open         SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    high         SimpleAggregateFunction(max, Decimal(38, 8)),
    low          SimpleAggregateFunction(min, Decimal(38, 8)),
    close        SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    volume       Decimal(38, 8),
    quote_volume Decimal(38, 8),
    trade_count  UInt64
)
ENGINE = SummingMergeTree()
PARTITION BY toYYYYMM(bucket)
ORDER BY (symbol, bucket)
TTL bucket + INTERVAL 5 YEAR;

CREATE TABLE IF NOT EXISTS exchange_analytics.ohlcv_1mo
(
    bucket       DateTime('UTC'),
    symbol       LowCardinality(String),
    open         SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    high         SimpleAggregateFunction(max, Decimal(38, 8)),
    low          SimpleAggregateFunction(min, Decimal(38, 8)),
    close        SimpleAggregateFunction(anyLast, Decimal(38, 8)),
    volume       Decimal(38, 8),
    quote_volume Decimal(38, 8),
    trade_count  UInt64
)
ENGINE = SummingMergeTree()
PARTITION BY toYYYYMM(bucket)
ORDER BY (symbol, bucket)
TTL bucket + INTERVAL 5 YEAR;

-- Convenience union: one queryable surface over all 12 persisted
-- intervals. `interval` matches the canonical interval tokens
-- (1m,5m,15m,30m,1h,2h,4h,6h,8h,1D,1W,1M).
CREATE VIEW IF NOT EXISTS exchange_analytics.ohlcv AS
SELECT '1m'  AS interval, * FROM exchange_analytics.ohlcv_1m
UNION ALL SELECT '5m',  * FROM exchange_analytics.ohlcv_5m
UNION ALL SELECT '15m', * FROM exchange_analytics.ohlcv_15m
UNION ALL SELECT '30m', * FROM exchange_analytics.ohlcv_30m
UNION ALL SELECT '1h',  * FROM exchange_analytics.ohlcv_1h
UNION ALL SELECT '2h',  * FROM exchange_analytics.ohlcv_2h
UNION ALL SELECT '4h',  * FROM exchange_analytics.ohlcv_4h
UNION ALL SELECT '6h',  * FROM exchange_analytics.ohlcv_6h
UNION ALL SELECT '8h',  * FROM exchange_analytics.ohlcv_8h
UNION ALL SELECT '1D',  * FROM exchange_analytics.ohlcv_1d
UNION ALL SELECT '1W',  * FROM exchange_analytics.ohlcv_1w
UNION ALL SELECT '1M',  * FROM exchange_analytics.ohlcv_1mo;
