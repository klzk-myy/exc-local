-- 116_accounts_swapfree_status.down.sql
-- Bring-forward column only; migration 095 retains canonical ownership —
-- this down migration removes what 112 added.

BEGIN;

ALTER TABLE accounts DROP CONSTRAINT IF EXISTS chk_accounts_swapfree_status;
ALTER TABLE accounts DROP COLUMN IF EXISTS swapfree_status;

COMMIT;
