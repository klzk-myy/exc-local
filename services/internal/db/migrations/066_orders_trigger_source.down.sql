-- 066_orders_trigger_source.down.sql
-- Phase-16 Task 16.3.17 — drop orders.trigger_source.

BEGIN;

ALTER TABLE orders
    DROP COLUMN trigger_source;

COMMIT;
