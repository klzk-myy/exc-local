-- 227_sor_shadow_orders.down.sql — Phase-18 SOR shadow tables.

BEGIN;

DROP TABLE IF EXISTS sor_fill_dedup;
DROP TABLE IF EXISTS sor_shadow_orders;

COMMIT;
