-- 002_trades.sql — enriched trade-fill records (spec §16.1, Task 20.3.1).
--
-- Same wire source as ticks (JetStream `trades` stream) plus the
-- order-level lineage the reporting queries need (Task 20.3.5 reads
-- maker_account_id/taker_account_id for the per-tier rollup).
--
-- NAMING CAVEAT (deviation for §27): the wire TradeFill carries
-- buy_order_id/sell_order_id, NOT maker/taker flags — the engine's
-- event does not identify which side rested. maker_account_id therefore
-- carries the BUY-side account and taker_account_id the SELL-side
-- account (resolved through the consumer's order_id -> account index
-- fed by OrderNew events on the `analytics` stream; 0 = unresolved).
-- Task 20.3.5's TradesPerTier uses both columns symmetrically
-- (UNION ALL over both sides), so account attribution is exact even
-- though the maker/taker labels are really buy/sell.
--
-- Dedup: ReplacingMergeTree(ver) on (symbol, trade_id, event_seq) —
-- a re-published fill collapses to the newest version on merge (§16.6).
--
-- Retention: 5 years (MiFID II record-keeping, spec §19.12 warm tier);
-- the S3 cold tier retains the underlying artifacts 7 years.

CREATE TABLE IF NOT EXISTS exchange_analytics.trades
(
    ts               DateTime64(3, 'UTC'),
    symbol           LowCardinality(String),
    trade_id         UInt64,
    instrument_id    Int64,                  -- 0 = order index had no mapping
    maker_account_id Int64,                  -- buy-side account (see caveat)
    taker_account_id Int64,                  -- sell-side account (see caveat)
    buy_order_id     UInt64,
    sell_order_id    UInt64,
    price            Decimal(38, 8),
    qty              Decimal(38, 8),     -- read as `qty` by Task 20.3.5 TradesPerTier / income_test seed
    aggressor_side   LowCardinality(String), -- 'BUY'|'SELL'|'UNKNOWN' (wire lacks the flag)
    event_seq        UInt64,
    shard_id         UInt32,
    ver              UInt64
)
ENGINE = ReplacingMergeTree(ver)
PARTITION BY toYYYYMM(ts)
ORDER BY (symbol, trade_id, event_seq)
TTL ts + INTERVAL 5 YEAR
SETTINGS index_granularity = 8192;
