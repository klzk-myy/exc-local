-- 209_solvency_snapshots.down.sql — revert Phase-13 Task 13.3.7 tables.

BEGIN;

DROP TABLE IF EXISTS solvency_proofs;
DROP TABLE IF EXISTS solvency_snapshots;

COMMIT;
