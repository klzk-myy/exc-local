-- 049_client_statements.down.sql
BEGIN;

DROP TABLE IF EXISTS erp_delivery_log;
DROP TABLE IF EXISTS trial_balances;
DROP TABLE IF EXISTS fee_invoices;
DROP TABLE IF EXISTS trade_confirmations;
DROP TABLE IF EXISTS client_statements;

DROP TYPE IF EXISTS erp_delivery_status_enum;
DROP TYPE IF EXISTS invoice_status_enum;
DROP TYPE IF EXISTS confirmation_status_enum;
DROP TYPE IF EXISTS statement_period_enum;

COMMIT;
