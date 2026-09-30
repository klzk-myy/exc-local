-- 271_lp_accounts.down.sql

BEGIN;

DROP INDEX IF EXISTS lp_accounts_lp_idx;
DROP TABLE IF EXISTS lp_accounts;

COMMIT;
