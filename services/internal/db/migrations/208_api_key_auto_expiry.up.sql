-- 208_api_key_auto_expiry.up.sql
-- Phase-13 Task 13.3.8 — API-key auto-expiration policy bookkeeping.
--
-- Defense-in-depth against orphaned high-privilege keys: a daily policy
-- sweep strips the TRADE and TRANSFER scopes from api_keys older than 90
-- days that carry no ip_allowlist. The key row itself is never deleted —
-- the revocation is recorded, restorable state:
--
--   permissions_revoked_at — when the policy sweep stripped privileged
--                            scopes; NULL = privileges intact (or never
--                            held). Presence is the "revoked" marker.
--   revoked_scopes         — the exact scopes removed (subset of
--                            {trade,transfer}); restored verbatim when the
--                            key later gains an ip_allowlist.
--   expiry_notified_at     — when the T-7d warning notification was sent;
--                            gates re-notification so the sweep is
--                            idempotent.
--   expiry_override_until  — admin dual-control grace extension
--                            (PUT /api/v1/admin/api-keys/{id}/extend-expiry);
--                            while > now() the sweep leaves the key alone.
--
-- Key validity (status/revoked_at/expires_at) is untouched: a stripped key
-- still authenticates with its remaining scopes (read survives).

BEGIN;

ALTER TABLE api_keys
    ADD COLUMN permissions_revoked_at TIMESTAMPTZ,
    ADD COLUMN revoked_scopes         TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN expiry_notified_at     TIMESTAMPTZ,
    ADD COLUMN expiry_override_until  TIMESTAMPTZ;

-- Restore integrity: revoked_scopes may only carry the privileged pair
-- (the sweep never strips read/admin), and only when a revocation
-- timestamp exists.
ALTER TABLE api_keys
    ADD CONSTRAINT api_keys_revoked_scopes_consistent CHECK (
        (permissions_revoked_at IS NULL AND revoked_scopes = '{}') OR
        (permissions_revoked_at IS NOT NULL AND
         revoked_scopes <@ ARRAY['trade','transfer']::text[])
    );

-- Sweep scans: live privileged keys without an allowlist, ordered by age.
CREATE INDEX idx_api_keys_privilege_expiry ON api_keys (created_at)
    WHERE status = 'ACTIVE' AND revoked_at IS NULL
      AND permissions_revoked_at IS NULL;

-- Restore scans: revoked keys whose allowlist has since been configured.
CREATE INDEX idx_api_keys_permission_restore ON api_keys (id)
    WHERE permissions_revoked_at IS NOT NULL;

COMMIT;
