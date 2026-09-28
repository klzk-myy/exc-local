-- 025_create_api_keys.up.sql
-- Phase-05 Task 5.3.16 migration-note schema, extended for the consumers
-- that land on it before migration 073 (Task 5.3.38, asymmetric types
-- adds key_type / public_key / algorithm / rotates_from_id /
-- overlap_until):
--   key_id      — opaque public identifier presented as X-API-KEY; the
--                 auth lookup key (auth.KeyStore selects on it).
--   key_hash    — spec'd SHA-256 of the presented credential; nullable so
--                 asymmetric rows (073) needn't carry a secret hash.
--   secret_enc  — AES-256-GCM envelope (nonce||ct, via auth.SecretBox) of
--                 the HMAC shared secret. HMAC verification must recover
--                 the raw secret, so it is encrypted-not-hashed. NULL for
--                 asymmetric keys (073 stores public_key only — private
--                 keys never enter the platform, §24 #283).
--   scopes      — §8.8 scope vocabulary (read / trade / transfer / admin);
--                 sub-account keys (Task 5.3.11) are restricted to
--                 read/trade.
--   user_id     — issuing user (denormalized for auth forensics).
--   expires_at  — §8.8 auto-expiry; NULL = no expiry.
--   revoke_reason / last_used_* — rotation audit + usage telemetry used
--                 by auth.KeyStore.

BEGIN;

CREATE TABLE api_keys (
    id              BIGSERIAL PRIMARY KEY,
    key_id          VARCHAR(32) NOT NULL UNIQUE,       -- public identifier (X-API-KEY)
    account_id      BIGINT      NOT NULL REFERENCES accounts (id),
    user_id         BIGINT      NOT NULL REFERENCES users (id),
    key_hash        VARCHAR(64) UNIQUE,                -- hex SHA-256 of credential; NULL ok
    secret_enc      BYTEA,                             -- sealed HMAC shared secret
    key_prefix      VARCHAR(16) NOT NULL DEFAULT '',   -- display prefix; never the secret
    label           VARCHAR(128) NOT NULL DEFAULT '',
    scopes          TEXT[]      NOT NULL DEFAULT '{}',
    rate_limit_tier VARCHAR(32) NOT NULL DEFAULT 'STANDARD',
    ip_allowlist    TEXT[],                            -- IPs/CIDRs; NULL = unrestricted
    status          VARCHAR(16) NOT NULL DEFAULT 'ACTIVE',
    expires_at      TIMESTAMPTZ,
    last_used_at    TIMESTAMPTZ,
    last_used_ip    INET,
    revoke_reason   VARCHAR(32),
    created_by      BIGINT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at      TIMESTAMPTZ
    -- NOTE: no CHECK binding secret_enc↔key_type here — key_type arrives
    -- with migration 073, whose api_keys_key_material_consistent CHECK
    -- owns the HMAC-secret vs public-key invariant. Issuance services
    -- must still refuse HMAC keys without secret_enc (fail-closed).
);

-- Per-account listing scans live keys only.
CREATE INDEX idx_api_keys_account_active ON api_keys (account_id)
    WHERE status = 'ACTIVE';

COMMIT;
