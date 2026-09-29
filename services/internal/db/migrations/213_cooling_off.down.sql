-- Migration 213 down — drop cooling_off_periods.
BEGIN;
DROP TABLE IF EXISTS cooling_off_periods;
COMMIT;
