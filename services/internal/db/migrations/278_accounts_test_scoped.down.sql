-- 278_accounts_test_scoped.down.sql

BEGIN;

DROP INDEX IF EXISTS idx_accounts_test_scoped;
ALTER TABLE accounts DROP COLUMN test_scoped;

COMMIT;
