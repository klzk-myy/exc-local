-- 006_tca_results.sql — TCA engine output (Task 20.3.9, spec §24 #400,
-- MiFID II Art. 27 best-execution evidence). Forward-declared here so the
-- sibling writer (services/internal/analytics/tca_engine.go) has its
-- table.
--
-- One row per (account_id, instrument_id, fill_id, order_id, period):
-- `period` 'fill' for per-fill metrics and 'daily'/'monthly'/'quarterly'
-- for the aggregated RTS-28 granularity rows. Benchmark columns are
-- NULLABLE — an absent benchmark is recorded as NULL, never fabricated
-- (§2.7; fix_absent is derived read-side from ecb_fix IS NULL plus the
-- writer's own flag column convention — see tca_engine.go).
--
-- ReplacingMergeTree(ver) lets a recompute overwrite a stale aggregate
-- deterministically. No TTL — execution-quality evidence is
-- finance-retention (≥5y per §19.12).

CREATE TABLE IF NOT EXISTS exchange_analytics.tca_results
(
    account_id              Int64,
    instrument_id           Int64,
    symbol                  LowCardinality(String),
    fill_id                 Int64,
    order_id                Int64,
    exec_price              Decimal(38, 8),
    arrival_price           Nullable(Decimal(38, 8)),
    session_vwap            Nullable(Decimal(38, 8)),
    ecb_fix                 Nullable(Decimal(38, 8)),
    slip_arrival_bps        Nullable(Decimal(20, 6)),
    slip_vwap_bps           Nullable(Decimal(20, 6)),
    slip_fix_bps            Nullable(Decimal(20, 6)),
    price_improvement_delta Nullable(Decimal(38, 8)),
    period                  LowCardinality(String),  -- fill|daily|monthly|quarterly
    period_start            DateTime64(3, 'UTC'),
    ts                      DateTime64(3, 'UTC'),
    ver                     UInt64
)
ENGINE = ReplacingMergeTree(ver)
PARTITION BY toYYYYMM(ts)
ORDER BY (account_id, instrument_id, fill_id, order_id, period);
