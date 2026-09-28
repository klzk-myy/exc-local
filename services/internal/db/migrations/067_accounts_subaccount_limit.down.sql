-- 067_accounts_subaccount_limit.down.sql

BEGIN;

DROP INDEX IF EXISTS idx_accounts_parent;
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_max_sub_accounts_range;
ALTER TABLE accounts DROP COLUMN IF EXISTS max_sub_accounts;

COMMIT;
