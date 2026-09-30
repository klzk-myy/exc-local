-- 230_liquidation_risk.down.sql

BEGIN;

DROP TABLE IF EXISTS insurance_fund_adjustments;
DROP TABLE IF EXISTS insurance_fund_governance;
DROP TABLE IF EXISTS adl_directives;
DROP TABLE IF EXISTS nbp_events;
DROP TABLE IF EXISTS liquidation_events;
DROP TABLE IF EXISTS margin_call_events;
DROP TABLE IF EXISTS insurance_fund_transactions;
DROP INDEX IF EXISTS insurance_fund_currency_ux;

COMMIT;
