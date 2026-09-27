-- 103_orders_discretionary_offset.up.sql
-- Spec §6.11 / §24 #405 — discretionary offset limit orders (Phase-02 Task
-- 2.3.26): passive limit price + hidden aggressive price-improvement band
-- expressed in pips (DECIMAL(10,4) supports pipette precision).

BEGIN;

ALTER TABLE orders
    ADD COLUMN discretionary_offset_pips DECIMAL(10,4) NOT NULL DEFAULT 0.0;

COMMIT;
