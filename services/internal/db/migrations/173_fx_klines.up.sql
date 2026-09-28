-- 173_fx_klines.up.sql
-- Phase-05 Task 5.3.5 read side / Phase-06 Task 6.3.8 write side.
--
-- fx_klines is the pre-materialized OHLCV aggregate table the REST kline
-- endpoint (GET /api/v1/klines/{symbol}) MUST read per the spec §10.3
-- contract (remediation #38 — scanning fx_trades/trades or aggregating
-- candles in-request is prohibited). The Phase-06 candle engine is the
-- writer: it upserts the in-progress bar and finalizes on interval close
-- (closed=true). ClickHouse `candles` is the long-term analytical mirror;
-- this PG table is the low-latency REST read model for the trailing window.
--
-- timeframe uses the canonical 13-interval set (Phase-06 Task 6.3.8/6.3.14):
-- 1s (memory-only upstream, persisted rows permitted), 1m, 5m, 15m, 30m,
-- 1h, 2h, 4h, 6h, 8h, 1D, 1W, 1M.

BEGIN;

CREATE TABLE fx_klines (
    instrument_id BIGINT        NOT NULL,
    symbol        VARCHAR(32)   NOT NULL,               -- denormalized for REST lookup
    timeframe     VARCHAR(4)    NOT NULL,               -- 1s|1m|5m|15m|30m|1h|2h|4h|6h|8h|1D|1W|1M
    open_time     TIMESTAMPTZ   NOT NULL,               -- bucket start (UTC)
    open          DECIMAL(20,8) NOT NULL,
    high          DECIMAL(20,8) NOT NULL,
    low           DECIMAL(20,8) NOT NULL,
    close         DECIMAL(20,8) NOT NULL,
    volume        DECIMAL(28,8) NOT NULL DEFAULT 0,     -- base currency
    quote_volume  DECIMAL(28,8) NOT NULL DEFAULT 0,     -- quote currency
    trade_count   BIGINT        NOT NULL DEFAULT 0,
    closed        BOOLEAN       NOT NULL DEFAULT FALSE, -- false = in-progress bar
    updated_at    TIMESTAMPTZ   NOT NULL DEFAULT now(),
    PRIMARY KEY (instrument_id, timeframe, open_time)
);

CREATE INDEX idx_fx_klines_symbol ON fx_klines (symbol, timeframe, open_time DESC);

COMMIT;
