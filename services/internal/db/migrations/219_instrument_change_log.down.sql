-- 219_instrument_change_log.down.sql
BEGIN;

ALTER TABLE instruments
    DROP COLUMN IF EXISTS param_overrides;

DROP TRIGGER IF EXISTS trg_instrument_change_log_immutable ON instrument_change_log;
DROP FUNCTION IF EXISTS instrument_change_log_immutable();
DROP TABLE IF EXISTS instrument_change_log;
DROP TABLE IF EXISTS instrument_change_requests;

COMMIT;
