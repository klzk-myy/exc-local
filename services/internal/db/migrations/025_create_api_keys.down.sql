-- 025_create_api_keys.down.sql

BEGIN;

DROP INDEX IF EXISTS idx_api_keys_account_active;
DROP TABLE IF EXISTS api_keys;

COMMIT;
