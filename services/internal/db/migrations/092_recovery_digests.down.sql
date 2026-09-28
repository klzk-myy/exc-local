-- 092_recovery_digests.down.sql — drop the Task 4.3.11 digest table.

BEGIN;

DROP TABLE IF EXISTS recovery_digests;

COMMIT;
