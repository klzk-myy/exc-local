-- 001_create_instruments.down.sql
BEGIN;

DROP TABLE IF EXISTS instruments CASCADE;
DROP TYPE IF EXISTS instrument_status_enum;
DROP TYPE IF EXISTS instrument_type_enum;

COMMIT;
