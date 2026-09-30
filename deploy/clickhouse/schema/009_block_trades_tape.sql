-- Phase-23 Task 23.3.7 — public block-trade tape (spec §10.8/§16.1,
-- §24 #291, §28.1 Block Tape matrix row).
--
-- Columnar store behind GET /api/v1/history/block-trades/{symbol}.
-- Written by marketdata.BlockTapeStore: one PRINT row per delay-gated
-- publication (Phase-06 Task 6.3.20 producer sink), one
-- CORRECTION|BUST row per published correction (print→correction
-- lineage per spec §5.29 heritage).
--
-- ANONYMITY BY SCHEMA: there are no participant columns at all — no
-- side, no account ids, no order ids, no resting-liquidity fields.
-- The producer's wire payload (blockTradeData/blockCorrectionData)
-- carries none, so the tape cannot leak what it never stores.
--
-- entry_id encodes parity: print = block_trade_id<<1, correction =
-- block_trade_id<<1|1 (one executed correction per trade is the
-- §5.29 ceiling — trade_busts_executed_ux). ver collapses replays.
CREATE TABLE IF NOT EXISTS exchange_analytics.block_trades_tape
(
    entry_id           UInt64,
    kind               LowCardinality(String),  -- PRINT | CORRECTION | BUST
    block_trade_id     UInt64,                  -- corrections: print superseded
    symbol             LowCardinality(String),
    price              Decimal(38, 8),
    quantity           Decimal(38, 8),
    notional_usd       Decimal(38, 8),
    exec_ts            DateTime64(3, 'UTC'),    -- print: fill; correction: event
    pub_ts             DateTime64(3, 'UTC'),    -- publication instant (ordering axis)
    delay_ms           UInt64,
    venue_flags        Array(String),
    original_trade_id  UInt64,                  -- corrections only (0 on prints)
    corrected_price    Nullable(Decimal(38, 8)),
    corrected_quantity Nullable(Decimal(38, 8)),
    ver                UInt64
)
ENGINE = ReplacingMergeTree(ver)
PARTITION BY toYYYYMM(pub_ts)
ORDER BY (symbol, pub_ts, entry_id)
TTL pub_ts + INTERVAL 5 YEAR;  -- MiFID II record-keeping, spec §19.12
