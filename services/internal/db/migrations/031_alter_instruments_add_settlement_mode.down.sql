-- 031_alter_instruments_add_settlement_mode.down.sql
BEGIN;

ALTER TABLE instruments DROP COLUMN IF EXISTS settlement_mode;
DROP TYPE IF EXISTS settlement_mode_enum;

COMMIT;
