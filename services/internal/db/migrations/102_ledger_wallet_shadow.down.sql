-- 102_ledger_wallet_shadow.down.sql
DROP TABLE IF EXISTS journal_sums;
DROP TABLE IF EXISTS ledger_entries;
DROP TYPE IF EXISTS ledger_entry_type_enum;
DROP TYPE IF EXISTS ledger_direction_enum;
