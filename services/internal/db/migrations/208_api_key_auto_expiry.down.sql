-- 208_api_key_auto_expiry.down.sql — revert Phase-13 Task 13.3.8 columns.

BEGIN;

ALTER TABLE api_keys
    DROP CONSTRAINT IF EXISTS api_keys_revoked_scopes_consistent;
DROP INDEX IF EXISTS idx_api_keys_privilege_expiry;
DROP INDEX IF EXISTS idx_api_keys_permission_restore;
ALTER TABLE api_keys
    DROP COLUMN IF EXISTS permissions_revoked_at,
    DROP COLUMN IF EXISTS revoked_scopes,
    DROP COLUMN IF EXISTS expiry_notified_at,
    DROP COLUMN IF EXISTS expiry_override_until;

COMMIT;
