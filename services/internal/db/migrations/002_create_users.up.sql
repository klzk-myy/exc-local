-- 002_create_users.up.sql
-- Spec §5.16 users — baseline columns only.
-- Excluded (owned by later migrations): password_hash (027), anti_phishing_code.

BEGIN;

CREATE TYPE user_status_enum AS ENUM ('ACTIVE', 'SUSPENDED', 'CLOSED');
CREATE TYPE kyc_status_enum AS ENUM ('NONE', 'PENDING', 'VERIFIED', 'APPROVED', 'REJECTED', 'EXPIRED');

CREATE TABLE users (
    id          BIGSERIAL PRIMARY KEY,
    email       VARCHAR(255) NOT NULL UNIQUE,
    phone       VARCHAR(32),
    status      user_status_enum NOT NULL DEFAULT 'ACTIVE',
    totp_secret VARCHAR(64),                          -- encrypted at rest
    kyc_status  kyc_status_enum  NOT NULL DEFAULT 'NONE',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
