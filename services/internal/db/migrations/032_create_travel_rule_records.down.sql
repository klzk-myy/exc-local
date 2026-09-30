-- 032_create_travel_rule_records.down.sql
-- Phase-21 Task 21.3.2 — drops the FATF travel-rule record store.

BEGIN;

DROP INDEX IF EXISTS ix_travel_rule_account;
DROP INDEX IF EXISTS ix_travel_rule_status;
DROP INDEX IF EXISTS uq_travel_rule_transfer;
DROP TABLE IF EXISTS travel_rule_records;

COMMIT;
