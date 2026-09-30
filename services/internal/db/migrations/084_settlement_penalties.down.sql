-- 084_settlement_penalties.down.sql — drop Task 24.3.13 schema.

BEGIN;

DROP TABLE IF EXISTS buy_in_events;
DROP TABLE IF EXISTS settlement_penalties;
DROP TABLE IF EXISTS settlement_fails;

DROP TYPE IF EXISTS buyin_status_enum;
DROP TYPE IF EXISTS buyin_kind_enum;
DROP TYPE IF EXISTS penalty_status_enum;
DROP TYPE IF EXISTS penalty_direction_enum;
DROP TYPE IF EXISTS liquidity_class_enum;
DROP TYPE IF EXISTS settlement_fail_status_enum;
DROP TYPE IF EXISTS settlement_fail_regime_enum;

DROP INDEX IF EXISTS settlement_instructions_fail_ix;
ALTER TABLE settlement_instructions DROP COLUMN IF EXISTS fail_flag;

COMMIT;
