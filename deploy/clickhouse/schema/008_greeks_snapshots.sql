-- 008_greeks_snapshots.sql — historical Greeks matrix snapshots
-- (Phase-23 Task 23.3.5, spec §24 #250).
--
-- One row per (option contract × publish tick): the greeks@{symbol}
-- feed stores every fresh (non-stale) tick's matrix at the
-- HistoryEvery cadence (default 1s) through the marketdata
-- CHGreeksStore seam — frozen stale frames never write history
-- (storing them would accumulate duplicated stale snapshots and
-- masquerade as fresh computations, §2.7).
--
-- MergeTree ORDER BY (symbol, expiry, ts) per the task contract;
-- monthly partitions match the sibling analytics tables (001/003/004).
-- No TTL: backtesting depth is the product (§19.12 hot-warm-cold
-- tiering owns the long-horizon tiering decision).

CREATE TABLE IF NOT EXISTS exchange_analytics.greeks_snapshots
(
    ts          DateTime64(3, 'UTC'),
    symbol      LowCardinality(String),  -- option instrument symbol
    underlying  LowCardinality(String),  -- spot pair "EUR/USD"
    right       LowCardinality(String),  -- 'CALL' | 'PUT'
    style       LowCardinality(String),  -- 'EUROPEAN' | 'AMERICAN'
    model       LowCardinality(String),  -- 'GK' | 'LATTICE'
    expiry      DateTime64(3, 'UTC'),
    strike      Float64,
    delta       Float64,
    gamma       Float64,
    vega        Float64,                 -- per unit absolute vol
    theta       Float64,                 -- per year of decay
    rho         Float64,                 -- domestic (quote-ccy) rate
    rho_foreign Float64                  -- foreign (base-ccy) rate
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (symbol, expiry, ts)
SETTINGS index_granularity = 8192;
