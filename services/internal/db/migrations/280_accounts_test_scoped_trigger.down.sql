-- 280_accounts_test_scoped_trigger.down.sql
BEGIN;

DROP TRIGGER IF EXISTS accounts_mark_test_scoped ON accounts;
DROP FUNCTION IF EXISTS trg_accounts_mark_test_scoped();

COMMIT;
