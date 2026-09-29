-- 046_fix_sessions_entitlement.down.sql

BEGIN;

DROP INDEX IF EXISTS idx_fix_sessions_account;

ALTER TABLE fix_sessions
    DROP COLUMN IF EXISTS max_msgs_per_sec,
    DROP COLUMN IF EXISTS cancel_on_disconnect,
    DROP COLUMN IF EXISTS allowed_instruments,
    DROP COLUMN IF EXISTS api_key_id,
    DROP COLUMN IF EXISTS account_id;

COMMIT;
