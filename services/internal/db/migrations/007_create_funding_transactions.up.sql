-- 007_create_funding_transactions.up.sql
-- Spec §5.6 funding_transactions — full spec-canonical enum value sets
-- (later migrations extend via ALTER TYPE ... ADD VALUE IF NOT EXISTS).

BEGIN;

CREATE TYPE funding_type_enum   AS ENUM ('DEPOSIT', 'WITHDRAWAL', 'ADJUSTMENT', 'FEE', 'FUNDING_RATE', 'SETTLEMENT');
CREATE TYPE funding_status_enum AS ENUM ('PENDING', 'CONFIRMED', 'COMPLETED', 'FAILED', 'AUTO_CANCELLED', 'PENDING_REVIEW');
CREATE TYPE bank_method_enum    AS ENUM ('SWIFT', 'SEPA', 'FEDNOW', 'ACH', 'CHAPS', 'TARGET2', 'WIRE', 'INTERNAL');

CREATE TABLE funding_transactions (
    id                BIGSERIAL PRIMARY KEY,
    account_id        BIGINT NOT NULL,
    currency          VARCHAR(3) NOT NULL,
    type              funding_type_enum   NOT NULL,
    amount            DECIMAL(28,8)       NOT NULL,
    status            funding_status_enum NOT NULL DEFAULT 'PENDING',
    reference         VARCHAR(128),                    -- bank reference / transaction ID
    bank_method       bank_method_enum,
    reference_account VARCHAR(64),                     -- bank account / IBAN
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_at      TIMESTAMPTZ,
    completed_at      TIMESTAMPTZ
);

COMMIT;
