-- 008_create_withdrawal_confirmations.up.sql
-- Spec §5.7 withdrawal_confirmations.
-- Canonical confirm window: 15 minutes (AGENTS.md).
-- status enum values are lowercase per spec §5.7.

BEGIN;

CREATE TYPE withdrawal_confirmation_status_enum AS ENUM ('pending', 'confirmed', 'cancelled');

CREATE TABLE withdrawal_confirmations (
    id            BIGSERIAL PRIMARY KEY,
    withdrawal_id BIGINT NOT NULL REFERENCES funding_transactions (id),
    confirmed_by  BIGINT,                                   -- user_id
    method        VARCHAR(16),                              -- email, sms, push, 2fa_totp
    confirmed_at  TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ NOT NULL DEFAULT (now() + interval '15 minutes'),
    status        withdrawal_confirmation_status_enum NOT NULL DEFAULT 'pending'
);

COMMIT;
