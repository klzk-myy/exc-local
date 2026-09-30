-- 261_backoffice_nostro_recon.down.sql — Phase-24 Tasks 24.3.1/24.3.2.

BEGIN;

DROP TABLE IF EXISTS nostro_recon_breaks;
DROP TABLE IF EXISTS nostro_recon_runs;
DROP TABLE IF EXISTS nostro_statement_entries;
DROP TYPE IF EXISTS nostro_break_status_enum;
DROP TYPE IF EXISTS nostro_break_category_enum;
DROP TYPE IF EXISTS nostro_recon_status_enum;
DROP INDEX IF EXISTS nostro_accounts_ref_ux;
ALTER TABLE nostro_accounts DROP COLUMN IF EXISTS account_role;
DROP TYPE IF EXISTS nostro_account_role_enum;

COMMIT;
