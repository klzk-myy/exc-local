-- 108_suspense_accounts_routing
-- Phase-11 Task 11.3.11 (return-code mapping & third-party deposit fraud
-- quarantine); also consumed by Phase-24 Task 24.3.21 suspense routing.
-- Spec §5.46 (`suspense_account_mappings`), §17.16b (suspense GL routing).
--
-- Two tables:
--   1. suspense_account_mappings — spec §5.46 verbatim columns plus the
--      additive attribution columns the task text requires (remitter
--      name/account per Phase-24 Task 24.3.21 step 2, resolved account,
--      name-match score, rail, funding row and GL journal links).
--   2. rail_payments — outbound rail instructions and their persisted
--      message envelopes (Task 11.3.1 dispatch records + the pacs.004
--      automated return-wire record Task 11.3.11 mandates).
--
-- The GL suspense liability used by the deposit guard is
-- 2150_SUSPENSE_DEPOSITS_{CCY} (spec §5.46 default '2150' / §17.16b GL
-- account 2150, seeded by migration 088's chart). Phase-11/24 prose cites
-- a 2099 code — the spec chart is the contract; 2150 is canonical.

BEGIN;

CREATE TYPE unmatched_reason_enum AS ENUM
    ('MISSING_REFERENCE', 'UNKNOWN_BENEFICIARY', 'NAME_MISMATCH', 'AMOUNT_DISCREPANCY');

CREATE TYPE quarantine_status_enum AS ENUM
    ('QUARANTINED', 'INVESTIGATING', 'RESOLVED', 'RETURNED_TO_SOURCE');

CREATE TYPE rail_payment_status_enum AS ENUM
    ('PREPARED', 'DISPATCHED', 'ACKNOWLEDGED', 'SETTLED', 'RETURNED', 'FAILED', 'REJECTED');

CREATE TABLE suspense_account_mappings (
    id                        BIGSERIAL PRIMARY KEY,
    bank_tx_id                VARCHAR(128) NOT NULL UNIQUE,   -- bank-side transaction reference
    funding_transaction_id    BIGINT REFERENCES funding_transactions (id),
    account_id                BIGINT,                          -- resolved destination account, if any
    rail                      VARCHAR(16),                     -- SWIFT|SEPA|FEDNOW|ACH|CHAPS|TARGET2|WIRE
    currency                  VARCHAR(3)  NOT NULL,
    amount                    DECIMAL(28,8) NOT NULL,
    originator_name           VARCHAR(255),                    -- remitter name per wire
    originator_account        VARCHAR(64),                     -- remitter IBAN/account
    name_match_score          DECIMAL(4,3),                    -- Jaro-Winkler 0.000–1.000 (threshold 0.85)
    unmatched_reason          unmatched_reason_enum NOT NULL,
    gl_account                VARCHAR(32) NOT NULL DEFAULT '2150',
    quarantine_status         quarantine_status_enum NOT NULL DEFAULT 'QUARANTINED',
    quarantined_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    sla_expires_at            TIMESTAMPTZ NOT NULL,            -- 48h compliance review window
    assigned_investigator_id  BIGINT,
    resolution_notes          TEXT,
    resolved_at               TIMESTAMPTZ,
    journal_entry_id          BIGINT,                          -- GL suspense posting reference
    return_payment_id         BIGINT,                          -- rail_payments row for the return wire
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_suspense_status_sla
    ON suspense_account_mappings (quarantine_status, sla_expires_at);
CREATE INDEX idx_suspense_account
    ON suspense_account_mappings (account_id) WHERE account_id IS NOT NULL;

CREATE TABLE rail_payments (
    id                      BIGSERIAL PRIMARY KEY,
    funding_transaction_id  BIGINT REFERENCES funding_transactions (id),
    suspense_mapping_id     BIGINT REFERENCES suspense_account_mappings (id),
    direction               VARCHAR(8)  NOT NULL CHECK (direction IN ('OUTBOUND', 'RETURN')),
    rail                    VARCHAR(16) NOT NULL,              -- bank_method_enum domain minus INTERNAL
    message_type            VARCHAR(24) NOT NULL,              -- MT103|MT202|MT199|PAIN001|PACS008|PACS004|NACHA_FILE
    end_to_end_id           VARCHAR(64) NOT NULL UNIQUE,       -- ISO 20022 EndToEndId / ACH trace
    uetr                    VARCHAR(36),                       -- SWIFT UETR when applicable
    envelope                JSONB NOT NULL,                    -- canonical typed wire fields
    status                  rail_payment_status_enum NOT NULL DEFAULT 'PREPARED',
    return_code             VARCHAR(16),                       -- pacs.002 reason / ACH R-code
    return_reason           TEXT,
    value_date              DATE,
    dispatched_at           TIMESTAMPTZ,
    settled_at              TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_rail_payments_funding_tx
    ON rail_payments (funding_transaction_id) WHERE funding_transaction_id IS NOT NULL;
CREATE INDEX idx_rail_payments_status
    ON rail_payments (status, rail);

COMMIT;
