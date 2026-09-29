-- 047_otr_limits.up.sql
-- Phase-13 Task 13.3.6 — Order-to-Trade Ratio limits (MiFID II RTS 9).
--
-- risk_limits gains two columns resolved through the same
-- most-specific-wins lattice as every other column (account+symbol >
-- account+wildcard > tier+symbol > tier+wildcard > global):
--
--   max_order_to_trade_ratio — max allowed order events (new/modify/
--     cancel) per executed trade inside the rolling window. The breach
--     rule is  events > ratio * max(trades, 1).  Default 500 (spec
--     §13.6a canonical figure). NULL would mean "no OTR cap" — the
--     column is NOT NULL DEFAULT 500 so a scoped row that wants the
--     venue default needs no explicit value.
--
--   otr_window — the rolling evaluation window as a PostgreSQL
--     INTERVAL; default 60 seconds (canonical). LimitsService reads it
--     via EXTRACT(EPOCH FROM otr_window) so Go sees plain seconds.
--
-- Market-maker allowance (spec §9.6 / Task 18.3.10): the LP registry
-- (liquidity_providers, migration 191) carries no account linkage or
-- MM-program flag, so the honest mechanism is the existing risk_limits
-- scoping itself — a registered MM gets an account(+program-symbol)
-- scoped row with a higher max_order_to_trade_ratio. No flag column is
-- invented here.

BEGIN;

ALTER TABLE risk_limits
    ADD COLUMN IF NOT EXISTS max_order_to_trade_ratio DECIMAL(28,8)
        NOT NULL DEFAULT 500
        CHECK (max_order_to_trade_ratio > 0);

ALTER TABLE risk_limits
    ADD COLUMN IF NOT EXISTS otr_window INTERVAL
        NOT NULL DEFAULT interval '60 seconds'
        CHECK (otr_window > interval '0 seconds');

COMMIT;
