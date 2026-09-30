-- 261_backoffice_nostro_recon.up.sql
-- Phase-24 Tasks 24.3.1/24.3.2 — nostro/vostro account roles + the
-- nostro reconciliation store (spec §17; §24 #21, #1-#8). Allocated at
-- 261: the write scope's "next free ≥258" slot churned under parallel
-- Phase-24 landings (258–260 claimed by sibling tasks); 261 is free.
--
--   1. nostro_accounts.account_role — NOSTRO (our money at the
--      correspondent) vs VOSTRO (the correspondent's money held by us),
--      spec §17.1. Pre-24.3.1 rows are all nostro — the DEFAULT
--      backfills honestly.
--   2. nostro_accounts_ref_ux — one registry row per
--      (currency, bank_code, account_number); re-creating the same
--      correspondent account is rejected (409 NOSTRO_ACCOUNT_EXISTS)
--      instead of double-counting balances.
--   3. nostro_statement_entries — bank-statement lines (the Task 24.3.12
--      MT940/MT942/camt.053 parsers and the Phase-11 polling seam both
--      land here). Idempotent ingest on
--      (account, statement_date, reference, direction, amount).
--   4. nostro_recon_runs — one row per (account, date) reconciliation
--      pass; reruns append a new row (audit-friendly — the report reads
--      the latest per account) and carry the §24 #21 threshold flag.
--   5. nostro_recon_breaks — per-item exceptions feeding the
--      investigation workflow (OPEN → INVESTIGATING → RESOLVED |
--      AUTO_RESOLVED).

BEGIN;

CREATE TYPE nostro_account_role_enum AS ENUM ('NOSTRO', 'VOSTRO');

ALTER TABLE nostro_accounts
    ADD COLUMN IF NOT EXISTS account_role nostro_account_role_enum NOT NULL DEFAULT 'NOSTRO';

-- One row per (currency, bank code, account number); COALESCE keeps the
-- index functional while bank_code/account_number stay nullable.
CREATE UNIQUE INDEX nostro_accounts_ref_ux
    ON nostro_accounts (currency, COALESCE(bank_code, ''),
                        COALESCE(account_number, ''), COALESCE(iban, ''));

CREATE TABLE nostro_statement_entries (
    id                BIGSERIAL PRIMARY KEY,
    nostro_account_id BIGINT      NOT NULL REFERENCES nostro_accounts (id),
    statement_date    DATE        NOT NULL,             -- bank value date
    swift_reference   VARCHAR(64) NOT NULL DEFAULT '',  -- :20:/:21:/bank ref ('' when unreferenced)
    direction         nostro_movement_direction_enum NOT NULL, -- DEBIT|CREDIT (reuse 112 enum)
    amount            DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    currency          VARCHAR(3)  NOT NULL,
    closing_balance   DECIMAL(28,8),                    -- statement running balance when reported
    narrative         VARCHAR(256),                     -- :86: remittance/charge text
    source            VARCHAR(16) NOT NULL DEFAULT 'MT940', -- MT940|MT942|CAMT053|POLL|MANUAL
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (nostro_account_id, statement_date, swift_reference, direction, amount)
);

CREATE INDEX nostro_statement_entries_date_ix
    ON nostro_statement_entries (nostro_account_id, statement_date);

CREATE TYPE nostro_recon_status_enum AS ENUM ('CLEAN', 'BREAKS_OPEN', 'DISCREPANCY');

CREATE TABLE nostro_recon_runs (
    id                   BIGSERIAL PRIMARY KEY,
    nostro_account_id    BIGINT      NOT NULL REFERENCES nostro_accounts (id),
    recon_date           DATE        NOT NULL,
    our_net              DECIMAL(28,8) NOT NULL,  -- signed net of our nostro_movements that day
    statement_net        DECIMAL(28,8) NOT NULL,  -- signed net of bank statement entries
    difference           DECIMAL(28,8) NOT NULL,  -- statement_net - our_net
    -- §24 #21: abs(difference) > $1,000-equivalent OR > 0.01% of expected.
    threshold_breach     BOOLEAN     NOT NULL DEFAULT false,
    breaks_opened        INT         NOT NULL DEFAULT 0,
    breaks_auto_resolved INT         NOT NULL DEFAULT 0,
    status               nostro_recon_status_enum NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Report reads the latest run per account for the day.
CREATE INDEX nostro_recon_runs_date_ix
    ON nostro_recon_runs (recon_date, nostro_account_id, id DESC);

CREATE TYPE nostro_break_category_enum AS ENUM
    ('TIMING', 'MISSING_CONFIRMATION', 'FEES', 'AMOUNT_MISMATCH', 'UNMATCHED');
CREATE TYPE nostro_break_status_enum AS ENUM
    ('OPEN', 'INVESTIGATING', 'AUTO_RESOLVED', 'RESOLVED');

CREATE TABLE nostro_recon_breaks (
    id                  BIGSERIAL PRIMARY KEY,
    run_id              BIGINT      NOT NULL REFERENCES nostro_recon_runs (id),
    nostro_account_id   BIGINT      NOT NULL REFERENCES nostro_accounts (id),
    recon_date          DATE        NOT NULL,
    category            nostro_break_category_enum NOT NULL,
    nostro_movement_id  BIGINT      REFERENCES nostro_movements (id),
    statement_entry_id  BIGINT      REFERENCES nostro_statement_entries (id),
    swift_reference     VARCHAR(64),
    currency            VARCHAR(3),
    expected_amount     DECIMAL(28,8),
    actual_amount       DECIMAL(28,8),
    difference          DECIMAL(28,8),
    status              nostro_break_status_enum NOT NULL DEFAULT 'OPEN',
    assigned_to         BIGINT,                     -- investigation workflow owner
    resolution_notes    TEXT,
    detected_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at         TIMESTAMPTZ,
    resolved_by         BIGINT
);

-- One break row per (run, item) pair — expression index so NULL sides
-- still dedupe.
CREATE UNIQUE INDEX nostro_recon_breaks_item_ux
    ON nostro_recon_breaks (run_id,
                            COALESCE(nostro_movement_id, 0),
                            COALESCE(statement_entry_id, 0));
CREATE INDEX nostro_recon_breaks_open_ix
    ON nostro_recon_breaks (nostro_account_id, recon_date)
    WHERE status IN ('OPEN', 'INVESTIGATING');

COMMIT;
