-- 119_commission_engine.down.sql — reverse Task 3.3.13 schema.

BEGIN;

DROP TABLE IF EXISTS account_monthly_volume;
DROP TABLE IF EXISTS commission_tiers;

COMMIT;
