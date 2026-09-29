-- 040_bank_accounts.up.sql
-- Spec §5.23 bank_accounts (Beneficiary Registry), Phase-11 Task 11.3.7.
--
-- Withdrawals may only target a registered, VERIFIED beneficiary;
-- deposits from unregistered senders are flagged for third-party review.
--
-- Reconciliation note (Task 11.3.7 task text vs spec §5.23 prose): the
-- task names the PK `bank_account_id`, a `status` lifecycle enum
-- (PENDING_VERIFICATION | VERIFIED | REJECTED) and `verified_by`; the
-- spec prose instead described `id` + `verified BOOLEAN`. The status
-- lifecycle is strictly more expressive (REJECTED is distinct from
-- never-verified), so this migration implements the task text and the
-- spec §5.23 table is updated to match. `bic_routing` (spec column)
-- carries routing/sort-code data; `swift_bic` (task column) carries the
-- SWIFT BIC — both retained.
--
-- unlocked_at implements the §24 #391 / Task 11.3.10-style 24-hour
-- new-beneficiary hold: set to verified_at + 24h by the verification
-- path; withdrawals to the destination are refused until it lapses.

BEGIN;

CREATE TYPE bank_account_status_enum AS ENUM (
    'PENDING_VERIFICATION', 'VERIFIED', 'REJECTED'
);

CREATE TYPE bank_account_rail_enum AS ENUM (
    'SWIFT', 'SEPA', 'FEDNOW', 'ACH', 'CHAPS', 'TARGET2', 'WIRE', 'INTERNAL'
);

CREATE TABLE bank_accounts (
    bank_account_id      BIGSERIAL PRIMARY KEY,
    account_id           BIGINT NOT NULL REFERENCES accounts(id),
    currency             VARCHAR(3)   NOT NULL,
    iban                 VARCHAR(34),                       -- NULL if non-IBAN
    account_number       VARCHAR(64),
    swift_bic            VARCHAR(16),
    bic_routing          VARCHAR(32),                       -- routing / sort code
    bank_name            VARCHAR(128) NOT NULL,
    beneficiary_name     VARCHAR(255) NOT NULL,             -- must match KYC legal name
    rail                 bank_account_rail_enum NOT NULL,
    status               bank_account_status_enum NOT NULL DEFAULT 'PENDING_VERIFICATION',
    verification_method  VARCHAR(24),                       -- MICRO_DEPOSIT | BANK_STATEMENT
    verified_at          TIMESTAMPTZ,
    verified_by          BIGINT,                            -- admin user_id (never self-verify)
    unlocked_at          TIMESTAMPTZ,                       -- verified_at + 24h hold
    rejection_reason     TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT bank_accounts_dest_chk
        CHECK (iban IS NOT NULL OR account_number IS NOT NULL),
    CONSTRAINT bank_accounts_verify_chk
        CHECK (status <> 'VERIFIED' OR (verified_at IS NOT NULL AND verified_by IS NOT NULL))
);

CREATE INDEX idx_bank_accounts_account
    ON bank_accounts (account_id);
CREATE INDEX idx_bank_accounts_status
    ON bank_accounts (status) WHERE status = 'PENDING_VERIFICATION';
-- Destination lookup for the withdrawal allowlist gate and the
-- third-party deposit sender check.
CREATE INDEX idx_bank_accounts_iban
    ON bank_accounts (account_id, iban) WHERE iban IS NOT NULL;
CREATE INDEX idx_bank_accounts_acctnum
    ON bank_accounts (account_id, account_number) WHERE account_number IS NOT NULL;

COMMIT;
