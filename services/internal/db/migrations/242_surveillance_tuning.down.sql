-- 242_surveillance_tuning.down.sql — Phase-21 Task 21.3.27.

BEGIN;

DROP TABLE IF EXISTS surveillance_signal_tuning;
DROP TABLE IF EXISTS reporting_values;

-- Remove only this migration's class-default seed rows — operator-
-- authored cftc_position_limits rows (any other source) are kept.
DELETE FROM cftc_position_limits
 WHERE source = 'VENUE-CALIBRATION-21.3.27'
   AND instrument_id IS NULL
   AND instrument_type = 'SWAP';

COMMIT;
