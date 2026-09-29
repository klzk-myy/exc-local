-- 095_account_products_swapfree.down.sql
-- Reverse of 095: drops the verification records, the profile table and
-- the account-side columns. IF EXISTS guards keep the swapfree_status
-- teardown safe to interleave with migration 116's bring-forward
-- (whichever down runs first removes it; the second is a no-op).

BEGIN;

DROP TABLE IF EXISTS swapfree_verifications;

ALTER TABLE accounts
    DROP CONSTRAINT IF EXISTS chk_accounts_swapfree_status,
    DROP COLUMN IF EXISTS swapfree_status,
    DROP COLUMN IF EXISTS product_profile_id;

DROP TABLE IF EXISTS account_product_profiles;

COMMIT;
