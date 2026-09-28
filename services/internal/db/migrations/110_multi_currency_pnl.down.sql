-- 110_multi_currency_pnl.down.sql
BEGIN;

DELETE FROM chart_of_accounts WHERE account_code LIKE '4040_REALIZED_TRADING_PNL_%';
DROP TABLE IF EXISTS currency_conversions;
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS chk_accounts_pnl_settlement_mode;
ALTER TABLE accounts DROP COLUMN IF EXISTS pnl_settlement_mode;
ALTER TABLE accounts DROP COLUMN IF EXISTS base_currency;

COMMIT;
