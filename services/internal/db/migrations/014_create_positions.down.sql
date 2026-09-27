-- 014_create_positions.down.sql
BEGIN;

DROP TABLE IF EXISTS positions CASCADE;
DROP TYPE IF EXISTS position_side_enum;

COMMIT;
