-- 198_funding_fee_schedule.down.sql — reverse Phase-11 Task 11.3.9 schema.

BEGIN;

DROP TABLE IF EXISTS funding_currency_conversions;
DROP TABLE IF EXISTS funding_fee_free_usage;
DROP TABLE IF EXISTS funding_fee_tiers;

COMMIT;
