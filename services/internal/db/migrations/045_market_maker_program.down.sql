-- 045_market_maker_program.down.sql
-- Phase-18 Task 18.3.10 — drop the MM program tables.

BEGIN;

DROP TABLE IF EXISTS mm_rebate_accruals;
DROP TABLE IF EXISTS mm_compliance;
DROP TABLE IF EXISTS mm_programs;
DROP TYPE IF EXISTS mm_program_status_enum;

COMMIT;
