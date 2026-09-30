-- 056_client_money.down.sql
BEGIN;

DROP TABLE IF EXISTS settlement_quarantines;
DROP TABLE IF EXISTS client_money_pooling_exports;
DROP TABLE IF EXISTS client_money_stress_runs;
DROP TABLE IF EXISTS client_money_regulator_notices;
DROP TABLE IF EXISTS client_money_remediations;
DROP TABLE IF EXISTS client_money_breaks;
DROP TABLE IF EXISTS client_money_reconciliations;
DROP TABLE IF EXISTS client_money_receipts;
DROP TABLE IF EXISTS client_money_bank_reviews;
DROP TABLE IF EXISTS client_money_accounts;

COMMIT;
