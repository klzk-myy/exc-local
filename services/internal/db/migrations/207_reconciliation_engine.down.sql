-- 207_reconciliation_engine.down.sql
BEGIN;

DROP TABLE IF EXISTS reconciliation_findings;
DROP TABLE IF EXISTS reconciliation_runs;

COMMIT;
