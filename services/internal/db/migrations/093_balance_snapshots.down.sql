-- 093_balance_snapshots.down.sql — drop the Task 20.3.13 snapshot table.

BEGIN;

DROP TABLE IF EXISTS balance_snapshots;

COMMIT;
