-- 068_webauthn_credentials.up.sql
-- Phase-12 Task 12.3.7: WebAuthn / Passkeys / FIDO2 credential store.
--
-- Pinned column contract (spec §12.6 / phase doc):
--   {id, user_id, credential_id, public_key, sign_count, transports,
--    created_at, last_used_at, name}
--
-- Extension columns beyond the pinned set (consistent with the additive
-- convention of 067):
--   aaguid      — authenticator model GUID from attestation (metadata/audit).
--   flags       — authenticator flags byte captured at registration. The
--                 WebAuthn login ceremony rejects assertions whose BE/BS
--                 flags differ from registration state, so the byte must be
--                 persisted; without it a synced passkey (BE=1) could never
--                 log in.
--   revoked_at  — clone-detection deactivation (Task 12.3.12): rows are
--                 deactivated, never deleted, preserving the forensic
--                 record of the compromised credential.
--
-- credential_id is globally unique (a credential id identifies one public
-- key pair per WebAuthn §6.1); the partial index serves active-credential
-- lookups for a user's ceremonies.

BEGIN;

CREATE TABLE webauthn_credentials (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT      NOT NULL REFERENCES users (id),
    credential_id BYTEA       NOT NULL UNIQUE,
    public_key    BYTEA       NOT NULL,                    -- COSE_Key (CBOR) encoding
    sign_count    BIGINT      NOT NULL DEFAULT 0,
    transports    TEXT[]      NOT NULL DEFAULT '{}',
    aaguid        BYTEA,
    flags         SMALLINT    NOT NULL DEFAULT 0,
    name          VARCHAR(128) NOT NULL DEFAULT '',
    revoked_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ,
    CONSTRAINT webauthn_credentials_sign_count_range
        CHECK (sign_count >= 0 AND sign_count <= 4294967295)  -- uint32
);

-- Per-user credential listing (account security page, ceremony loading).
CREATE INDEX idx_webauthn_credentials_user
    ON webauthn_credentials (user_id)
    WHERE revoked_at IS NULL;

COMMIT;
