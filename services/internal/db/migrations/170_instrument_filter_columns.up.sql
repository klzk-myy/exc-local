-- 170_instrument_filter_columns.up.sql
-- Phase-05 Task 5.3.35 (structured instrument filter objects) / Task 5.3.44
-- (unified venue-info document): per-instrument validation-rule columns the
-- public `filters` array surfaces. Existing columns already cover
-- LOT_SIZE (min_order_qty/max_order_qty/lot_size), MIN_NOTIONAL
-- (min_notional, migration 050) and PRICE_BAND (price_band_pct_up/down).
-- This migration adds the remainder:
--   min_price / max_price   — PRICE_FILTER bounds (NULL = unbounded;
--                             seeds pin min_price = tick_size)
--   max_spread_pips         — SPREAD_PROTECTION; spec §6.6 pins the column
--                             ("each instrument configures max_spread_pips",
--                             examples: 50 pips majors / 200 exotics) but §5.1
--                             never declared it — closing that schema gap here
--   max_open_orders         — MAX_ORDERS resting-order cap per instrument
--                             (account-scoped caps still live in
--                             risk_limits.max_open_orders, spec §5.10)
--   max_algo_orders         — MAX_ORDERS working algo-order cap
-- All values are strings of the fixed-point DECIMAL contract (§5.3 rule 1).

BEGIN;

ALTER TABLE instruments
    ADD COLUMN min_price        DECIMAL(20,8),
    ADD COLUMN max_price        DECIMAL(20,8),
    ADD COLUMN max_spread_pips  DECIMAL(12,4),
    ADD COLUMN max_open_orders  INTEGER,
    ADD COLUMN max_algo_orders  INTEGER;

-- Seed the venue defaults for the migration-001 instruments:
-- majors get the 50-pip spread cap, exotic USD/MXN the 200-pip cap
-- (spec §6.6 examples). min_price = tick_size (no price below one tick);
-- max_price stays NULL = unbounded. Order-count ceilings: 200 resting /
-- 50 working algo orders (Binance-parity venue defaults).
UPDATE instruments SET
    min_price        = tick_size,
    max_spread_pips  = CASE WHEN symbol = 'USD/MXN' THEN 200 ELSE 50 END,
    max_open_orders  = 200,
    max_algo_orders  = 50;

COMMIT;
