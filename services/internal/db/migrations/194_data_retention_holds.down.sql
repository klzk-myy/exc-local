-- 194_data_retention_holds.down.sql
BEGIN;

DROP TABLE IF EXISTS retention_audit_log;
DROP TABLE IF EXISTS data_retention_holds;

COMMIT;
