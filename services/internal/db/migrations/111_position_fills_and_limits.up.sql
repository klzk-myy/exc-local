-- 111_position_fills_and_limits.up.sql
-- Phase-03 Task 3.3.2 (Position Management).
--
-- 1. position_fills: per-account fill-application log. One engine trade
--    produces TWO position updates (buyer + seller), so the dedup key is
--    (trade_id, account_id) — deliberately independent of the
--    processed_trades table (migration 022, Task 3.3.1) which dedups the
--    balance-update pass keyed on trade_id alone.
-- 2. uq_positions_account_instrument: net-position invariant — at most one
--    position row per (account_id, instrument_id). FLAT is represented as
--    quantity = 0 (position_side_enum has no FLAT value; spec §5.13).
-- 3. risk_limits.max_open_positions: per-account position-count ceiling
--    consulted by PositionService (NULL = fall back to global default row,
--    then to the service default).

BEGIN;

CREATE TABLE IF NOT EXISTS position_fills (
    trade_id      BIGINT         NOT NULL,
    account_id    BIGINT         NOT NULL,
    instrument_id BIGINT         NOT NULL,
    side          VARCHAR(4)     NOT NULL CHECK (side IN ('BUY', 'SELL')),
    quantity      DECIMAL(28,8)  NOT NULL CHECK (quantity > 0),
    price         DECIMAL(20,8)  NOT NULL CHECK (price > 0),
    realized_pnl  DECIMAL(28,8)  NOT NULL DEFAULT 0,  -- quote-currency delta this fill realized
    applied_at    TIMESTAMPTZ    NOT NULL DEFAULT now(),
    PRIMARY KEY (trade_id, account_id)
);

CREATE INDEX IF NOT EXISTS idx_position_fills_account ON position_fills (account_id, applied_at);

CREATE UNIQUE INDEX IF NOT EXISTS uq_positions_account_instrument
    ON positions (account_id, instrument_id);

ALTER TABLE risk_limits
    ADD COLUMN IF NOT EXISTS max_open_positions INTEGER;

COMMIT;
