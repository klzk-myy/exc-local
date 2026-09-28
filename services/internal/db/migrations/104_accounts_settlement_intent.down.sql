-- 104_accounts_settlement_intent.down.sql

BEGIN;

ALTER TABLE orders   DROP COLUMN IF EXISTS settlement_intent;
ALTER TABLE accounts DROP COLUMN IF EXISTS settlement_intent;
DROP TYPE IF EXISTS settlement_intent_enum;

COMMIT;
