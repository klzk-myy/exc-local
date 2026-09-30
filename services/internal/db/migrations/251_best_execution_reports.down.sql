-- 251_best_execution_reports.down.sql — drop the RTS 27/28 stores.

BEGIN;

DROP TABLE IF EXISTS rts28_reports;
DROP TABLE IF EXISTS rts27_reports;
DROP TABLE IF EXISTS rts27_daily_stats;

COMMIT;
