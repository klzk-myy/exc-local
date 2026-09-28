-- 036_create_general_ledger.down.sql — reverse spec §5.21 GL schema.

BEGIN;

DROP TABLE IF EXISTS ledger_lines;
DROP TABLE IF EXISTS journal_entries;
DROP FUNCTION IF EXISTS gl_check_journal_balance();
DROP TABLE IF EXISTS chart_of_accounts;
DROP TYPE IF EXISTS gl_entry_type_enum;
DROP TYPE IF EXISTS gl_account_type_enum;

COMMIT;
