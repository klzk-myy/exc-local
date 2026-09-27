-- 050_instruments_min_notional.down.sql
BEGIN;

ALTER TABLE instruments DROP COLUMN IF EXISTS min_notional;

COMMIT;
