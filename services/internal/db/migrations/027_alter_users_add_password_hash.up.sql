-- 027_alter_users_add_password_hash.up.sql
-- Spec §5.16 users.password_hash (Phase-12 Task 12.3.1), plus the
-- self-service columns the Phase-12 auth/profile/2FA surface persists:
--   country             — registration residency, ISO 3166-1 alpha-2
--                         (POST /api/v1/auth/register)
--   full_name / address — self-declared contact profile (Task 12.3.3
--                         PUT /account/profile); verified legal PII
--                         (full_name/address/DOB) stays with
--                         kyc_profiles (§14.8, Phase-21 Task 21.3.1)
--   email_verified_at   — verification-ceremony timestamp (Task 12.3.1;
--                         NULL = unverified)
--   totp_backup_codes   — hex SHA-256 digests of the 10 single-use 2FA
--                         backup codes (Task 12.3.2); plaintext codes
--                         are shown once at enrollment and never stored

BEGIN;

ALTER TABLE users
    ADD COLUMN password_hash     VARCHAR(128),   -- bcrypt (60 chars; headroom for rehash params)
    ADD COLUMN country           VARCHAR(2),
    ADD COLUMN full_name         VARCHAR(128),
    ADD COLUMN address           VARCHAR(255),
    ADD COLUMN email_verified_at TIMESTAMPTZ,
    ADD COLUMN totp_backup_codes TEXT[] NOT NULL DEFAULT '{}';

-- totp_secret widened 64 → 160: the enrollment path (Task 12.3.2) and
-- accounts.PgxTOTPSecerts persist base64(SecretBox.Seal(seed)) — 80 chars
-- for the 160-bit seed, so VARCHAR(64) (sized for the plaintext base32
-- form) cannot hold the sealed contract. 160 covers seeds up to ~96B.
ALTER TABLE users ALTER COLUMN totp_secret TYPE VARCHAR(160);

COMMIT;
