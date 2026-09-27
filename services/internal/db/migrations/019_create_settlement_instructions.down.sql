-- 019_create_settlement_instructions.down.sql
BEGIN;

DROP TABLE IF EXISTS settlement_instructions CASCADE;
DROP TYPE IF EXISTS settlement_status_enum;
DROP TYPE IF EXISTS settlement_direction_enum;

COMMIT;
