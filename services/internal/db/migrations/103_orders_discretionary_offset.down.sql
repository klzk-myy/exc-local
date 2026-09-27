-- 103_orders_discretionary_offset.down.sql
BEGIN;

ALTER TABLE orders DROP COLUMN IF EXISTS discretionary_offset_pips;

COMMIT;
