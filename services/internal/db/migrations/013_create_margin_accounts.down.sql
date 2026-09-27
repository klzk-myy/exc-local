-- 013_create_margin_accounts.down.sql
BEGIN;

DROP TABLE IF EXISTS margin_accounts CASCADE;
DROP TYPE IF EXISTS margin_account_status_enum;
DROP TYPE IF EXISTS margin_mode_enum;

COMMIT;
