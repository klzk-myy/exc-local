-- 218_oco_group_link.down.sql — drop the OCO pair linkage column.

BEGIN;

DROP INDEX IF EXISTS idx_orders_oco_group;
ALTER TABLE orders DROP COLUMN IF EXISTS oco_group_id;

COMMIT;
