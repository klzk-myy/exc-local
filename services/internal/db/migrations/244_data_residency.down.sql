-- 244_data_residency.down.sql — reverses Task 21.3.18.

BEGIN;

DROP TRIGGER IF EXISTS trg_residency_access_guard ON data_residency_access_log;
DROP FUNCTION IF EXISTS data_residency_access_guard();

DROP INDEX IF EXISTS ix_accounts_jurisdiction;
ALTER TABLE accounts DROP COLUMN IF EXISTS jurisdiction_code;

DROP TABLE IF EXISTS data_residency_access_log;
DROP TABLE IF EXISTS data_residency_policies;

COMMIT;
