-- 040_bank_accounts.down.sql — drops the Task 11.3.7 beneficiary
-- registry table and its enums.

BEGIN;

DROP TABLE IF EXISTS bank_accounts;
DROP TYPE IF EXISTS bank_account_rail_enum;
DROP TYPE IF EXISTS bank_account_status_enum;

COMMIT;
