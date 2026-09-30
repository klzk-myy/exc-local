-- 239_account_consents.down.sql — drop the account consent gate table.

BEGIN;

DROP TABLE IF EXISTS account_consents;

COMMIT;
