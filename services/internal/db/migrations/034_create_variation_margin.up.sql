-- 034_create_variation_margin.up.sql
-- Phase-22 Task 22.3.7 (spec §5.25, §13.11, §15.7): daily variation-margin
-- settlement record for derivative positions (FORWARD/SWAP/NDF/OPTION).
--
-- settled_at is the idempotency watermark: a row exists for each claimed
-- (subject, settlement_date) settlement; settled_at NULL means the claim
-- was taken but the GL journal has not committed. The sweep skips rows
-- with settled_at NOT NULL (rerun same day = no double-post) and resumes
-- rows where a prior sweep died between claim and journal commit — the
-- journal idempotency key vm:{row_id}:{date} dedups the repost.
--
-- subject_kind/subject_ref identify the MTM source:
--   POSITION  -> positions.id      (instrument-netted margin positions)
--   CONTRACT  -> derivative_contracts.id (migration 254 booked contracts)
-- journal_entry_id links the settlement journal for audit.

BEGIN;

CREATE TABLE variation_margin (
    id               BIGSERIAL    PRIMARY KEY,
    account_id       BIGINT       NOT NULL REFERENCES accounts (id),
    instrument_id    BIGINT       NOT NULL REFERENCES instruments (id),
    subject_kind     VARCHAR(8)   NOT NULL CHECK (subject_kind IN ('POSITION', 'CONTRACT')),
    subject_ref      BIGINT       NOT NULL,
    settlement_date  DATE         NOT NULL,
    currency         VARCHAR(3)   NOT NULL,           -- settlement (quote) currency
    vm_amount        DECIMAL(28,8) NOT NULL,          -- signed client delta, MTM_today − MTM_prev
    mtm_value        DECIMAL(28,8) NOT NULL,          -- cumulative MTM watermark at settle
    vm_rate          DECIMAL(20,8),                   -- mark/fixing rate the MTM was computed at
    -- FK target lives in 036_create_general_ledger (journal_entries) —
    -- task numbering puts this file first, so the constraint is added by
    -- 276_deferred_variation_margin_journal_fk once the referenced table
    -- exists (a trailing file: subset fixtures apply 036 without 034).
    journal_entry_id BIGINT,
    shortfall        BOOLEAN      NOT NULL DEFAULT FALSE, -- wallet went negative after post
    settled_at       TIMESTAMPTZ,                     -- GL commit time; NULL = claimed only
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (subject_kind, subject_ref, settlement_date)
);

-- Unsettled claims a crashed sweep must resume.
CREATE INDEX variation_margin_unsettled_ix
    ON variation_margin (subject_ref)
    WHERE settled_at IS NULL;
-- Per-account/day audit queries.
CREATE INDEX variation_margin_account_day_ix
    ON variation_margin (account_id, settlement_date);

COMMIT;
