-- 094_accounts_default_stp.down.sql
BEGIN;

ALTER TABLE accounts DROP COLUMN IF EXISTS default_stp_mode;

COMMIT;
