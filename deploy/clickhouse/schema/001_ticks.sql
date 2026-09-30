-- 001_ticks.sql — raw trade ticks (spec §16.1, §16.6, §24 #65/#322).
--
-- Dedup invariant (§16.6): ReplacingMergeTree keyed on
-- (symbol, trade_id, event_seq) — the wire TradeFill carries no
-- instrument_id, so the subject's symbol token (or the UNKNOWN/instr-<id>
-- fallback domain) is the instrument identity; replayed inserts carry a
-- higher `ver` (ingest wall-clock nanos) and collapse deterministically on
-- merge / SELECT ... FINAL.
--
-- `side` is the taker/aggressor side. The wire TradeFill does not carry
-- an aggressor flag, so fills the engine emitted without one land as
-- 'UNKNOWN' — fail-closed: we never fabricate a side (§2.7).
--
-- Retention: raw ticks TTL 90 days (§16.1) — the 5-year retention applies
-- to the aggregated tables in 003/004, and the S3 cold tier (§19.12)
-- carries history past the TTL horizon.

CREATE TABLE IF NOT EXISTS exchange_analytics.ticks
(
    ts        DateTime64(3, 'UTC'),
    symbol    LowCardinality(String),
    price     Decimal(38, 8),
    quantity  Decimal(38, 8),            -- TCA VWAP reads sum(price*quantity)/sum(quantity)
    side      LowCardinality(String),    -- 'BUY' | 'SELL' | 'UNKNOWN'
    trade_id  UInt64,
    event_seq UInt64,
    shard_id  UInt32,
    ver       UInt64                     -- ingest batch version (unix nanos)
)
ENGINE = ReplacingMergeTree(ver)
PARTITION BY toYYYYMM(ts)
ORDER BY (symbol, trade_id, event_seq)
TTL ts + INTERVAL 90 DAY
SETTINGS index_granularity = 8192;
