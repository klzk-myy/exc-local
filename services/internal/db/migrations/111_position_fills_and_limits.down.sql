-- 111_position_fills_and_limits.down.sql
BEGIN;

ALTER TABLE risk_limits DROP COLUMN IF EXISTS max_open_positions;
DROP INDEX IF EXISTS uq_positions_account_instrument;
DROP TABLE IF EXISTS position_fills;

COMMIT;
