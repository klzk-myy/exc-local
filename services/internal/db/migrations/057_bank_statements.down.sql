-- 057_bank_statements — down

BEGIN;

DROP TABLE IF EXISTS statement_entries;
DROP TABLE IF EXISTS bank_statements;
DROP TYPE IF EXISTS statement_entry_status_enum;
DROP TYPE IF EXISTS bank_statement_status_enum;
DROP TYPE IF EXISTS bank_statement_format_enum;

COMMIT;
