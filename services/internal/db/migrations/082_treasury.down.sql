-- 082_treasury.down.sql
BEGIN;

DROP TABLE IF EXISTS treasury_controls;
DROP TABLE IF EXISTS treasury_liquidity_assessments;
DROP TABLE IF EXISTS contingent_capital_commitments;
DROP TABLE IF EXISTS own_funds_balances;

COMMIT;
