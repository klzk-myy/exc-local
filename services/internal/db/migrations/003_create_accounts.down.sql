-- 003_create_accounts.down.sql
BEGIN;

DROP TABLE IF EXISTS accounts CASCADE;
DROP TYPE IF EXISTS account_status_enum;
DROP TYPE IF EXISTS kyc_tier_enum;
DROP TYPE IF EXISTS account_type_enum;

COMMIT;
