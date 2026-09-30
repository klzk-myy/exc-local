-- 057_bank_statements
-- Phase-24 Task 24.3.12 — bank statement ingestion (MT940/MT942/camt.053)
-- and automated reconciliation breaks (spec §5.34, §17.10, §24 #175).
--
-- Tables:
--   1. bank_statements   — statement header (format, nostro account,
--      statement/sequence number, balances, sha256 file checksum as the
--      durable duplicate-transmission guard).
--   2. statement_entries — normalized per-entry records (entry ref, UETR,
--      end-to-end id, C/D indicator, reversal flag, match linkage).
--
-- Reconciliation breaks route into the shared `settlement_exceptions`
-- break queue (migration 258_settlement_ops — sibling-owned); the
-- statement-linkage columns on that table land in migration 260.

BEGIN;

CREATE TYPE bank_statement_format_enum AS ENUM ('MT940', 'MT942', 'CAMT053', 'CAMT052');
CREATE TYPE bank_statement_status_enum AS ENUM
    ('INGESTED', 'PARTIALLY_MATCHED', 'MATCHED', 'SEQUENCE_GAP', 'REJECTED');
CREATE TYPE statement_entry_status_enum AS ENUM
    ('UNMATCHED', 'MATCHED', 'EXCEPTION', 'REVERSED');

CREATE TABLE bank_statements (
    id                    BIGSERIAL PRIMARY KEY,
    nostro_account_id     BIGINT NOT NULL REFERENCES nostro_accounts (id),
    format                bank_statement_format_enum NOT NULL,
    statement_number      VARCHAR(32),               -- MT940/MT942 :28C: statement no
    sequence_number       INTEGER,                   -- :28C: sequence component
    statement_date        DATE,                      -- closing balance date (:62F:)
    opening_balance       DECIMAL(28,8),
    closing_balance       DECIMAL(28,8),
    currency              VARCHAR(3) NOT NULL,
    iban                  VARCHAR(34),               -- :25: / camt Acct IBAN
    bic                   VARCHAR(11),               -- sender / camt Svcr BICFI
    file_checksum_sha256  CHAR(64) NOT NULL UNIQUE,  -- duplicate-file transmission guard
    entry_count           INTEGER NOT NULL DEFAULT 0,
    matched_count         INTEGER NOT NULL DEFAULT 0,
    status                bank_statement_status_enum NOT NULL DEFAULT 'INGESTED',
    ingest_source         VARCHAR(16) NOT NULL DEFAULT 'MANUAL', -- SFTP | MQ | MANUAL
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX bank_statements_nostro_seq_ix
    ON bank_statements (nostro_account_id, statement_number, sequence_number);
CREATE INDEX bank_statements_date_ix ON bank_statements (statement_date);

CREATE TABLE statement_entries (
    id                        BIGSERIAL PRIMARY KEY,
    statement_id              BIGINT NOT NULL REFERENCES bank_statements (id) ON DELETE CASCADE,
    entry_ref                 VARCHAR(64),          -- :61: customer reference / NtryRef
    bank_ref                  VARCHAR(64),          -- :61: //bank reference / AcctSvcrRef
    uetr                      VARCHAR(36),          -- camt UETR
    end_to_end_id             VARCHAR(64),          -- camt EndToEndId
    value_date                DATE,
    booking_date              DATE,
    amount                    DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    currency                  VARCHAR(3) NOT NULL,
    credit_debit_indicator    VARCHAR(4) NOT NULL CHECK (credit_debit_indicator IN ('CRDT','DBIT')),
    is_reversal               BOOLEAN NOT NULL DEFAULT FALSE, -- MT940 RC/RD marks
    transaction_code          VARCHAR(16),          -- SWIFT tx type + idcode / proprietary code
    remitter_name             VARCHAR(255),
    remitter_account          VARCHAR(64),
    narrative                 TEXT,                  -- :86: / AddtlNtryInf
    -- match linkage (priority: UETR → transaction ref → amount+ccy+value date)
    reconciled_instruction_id BIGINT REFERENCES settlement_instructions (id),
    reconciled_payment_id     BIGINT REFERENCES rail_payments (id),
    reconciled_movement_id    BIGINT REFERENCES nostro_movements (id),
    match_key                 VARCHAR(8),           -- UETR | REF | AMOUNT
    status                    statement_entry_status_enum NOT NULL DEFAULT 'UNMATCHED',
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Idempotent re-ingest: the same entry inside a different (re-sent) file
-- is deduped on the natural key.
CREATE UNIQUE INDEX statement_entries_natural_ux
    ON statement_entries (statement_id, COALESCE(entry_ref,''), COALESCE(uetr,''), value_date, amount, credit_debit_indicator);
CREATE INDEX statement_entries_match_ix
    ON statement_entries (uetr) WHERE uetr IS NOT NULL;
CREATE INDEX statement_entries_status_ix
    ON statement_entries (status) WHERE status <> 'MATCHED';

COMMIT;
