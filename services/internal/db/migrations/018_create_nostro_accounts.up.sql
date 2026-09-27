-- 018_create_nostro_accounts.up.sql
-- Spec §5.18 nostro_accounts — correspondent bank accounts (§17 backoffice).

BEGIN;

CREATE TYPE nostro_status_enum AS ENUM ('ACTIVE', 'SUSPENDED', 'CLOSED');

CREATE TABLE nostro_accounts (
    id             BIGSERIAL PRIMARY KEY,
    currency       VARCHAR(3) NOT NULL,
    bank_name      VARCHAR(128) NOT NULL,                   -- correspondent bank
    bank_code      VARCHAR(32),                             -- SWIFT BIC / routing number
    account_number VARCHAR(64),
    iban           VARCHAR(34),                             -- NULL if non-IBAN
    balance        DECIMAL(28,8) NOT NULL DEFAULT 0,
    status         nostro_status_enum NOT NULL DEFAULT 'ACTIVE',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
