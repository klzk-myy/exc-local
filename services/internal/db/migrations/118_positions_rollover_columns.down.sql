-- 118_positions_rollover_columns.down.sql

BEGIN;

DROP TABLE IF EXISTS rollover_runs;

DROP INDEX IF EXISTS uq_swap_accrual_records_roll;
ALTER TABLE swap_accrual_records DROP COLUMN IF EXISTS roll_date;

ALTER TABLE positions DROP COLUMN IF EXISTS value_date;
ALTER TABLE positions DROP COLUMN IF EXISTS opened_at;

COMMIT;
