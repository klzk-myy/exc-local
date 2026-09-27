-- 050_instruments_min_notional.up.sql
-- Spec §5.1 instruments.min_notional — minimum order notional for the
-- pre-trade min-notional check (Phase-02 Task 2.3.3 check 12; §24 #156/#157).
-- DECIMAL(28,8) matches the quantity/price precision convention; 0 = no floor
-- (backward-compatible default for existing rows).

BEGIN;

ALTER TABLE instruments
    ADD COLUMN min_notional DECIMAL(28,8) NOT NULL DEFAULT 0;

COMMIT;
