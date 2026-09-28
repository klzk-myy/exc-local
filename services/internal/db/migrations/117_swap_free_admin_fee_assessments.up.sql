-- 117_swap_free_admin_fee_assessments.up.sql
-- Phase-03 Task 3.3.23 (spec §5.45.3 / §12.8a, §24 #407): per-position
-- audit ledger of assessed swap-free administrative holding fees.
--
-- Spec §5.45.3 describes this audit shape under the name
-- `swap_free_admin_fees` — but Task 3.3.23's implementation text assigns
-- that name to the per-instrument fee SCHEDULE (migration 105). This table
-- carries the §5.45.3 assessment columns under a disambiguated name; the
-- conflict is recorded as a deviation pending a spec §27 ruling.
--
-- Columns follow §5.45.3 verbatim (account_id, position_id, holding_days,
-- admin_fee_amount, currency, assessed_at, status ENUM('ASSESSED',
-- 'COLLECTED','WAIVED') DEFAULT 'ASSESSED') plus:
--   * roll_date  — the ET rollover date that produced the assessment;
--                  UNIQUE(position_id, roll_date) makes fee assessment
--                  idempotent across retried rollover runs (zero double
--                  charge).
--   * journal_entry_id — back-link to the balanced GL journal that
--                  collected the fee (NULL while ASSESSED/WAIVED).

BEGIN;

CREATE TYPE swap_free_fee_status_enum AS ENUM ('ASSESSED', 'COLLECTED', 'WAIVED');

CREATE TABLE swap_free_admin_fee_assessments (
    id               BIGSERIAL PRIMARY KEY,
    account_id       BIGINT        NOT NULL REFERENCES accounts (id),
    position_id      BIGINT        NOT NULL REFERENCES positions (id),
    holding_days     INTEGER       NOT NULL CHECK (holding_days >= 0),
    admin_fee_amount DECIMAL(28,8) NOT NULL CHECK (admin_fee_amount >= 0),
    currency         VARCHAR(3)    NOT NULL DEFAULT 'USD',
    roll_date        DATE          NOT NULL,
    journal_entry_id BIGINT        REFERENCES journal_entries (id),
    assessed_at      TIMESTAMPTZ   NOT NULL DEFAULT now(),
    status           swap_free_fee_status_enum NOT NULL DEFAULT 'ASSESSED',
    UNIQUE (position_id, roll_date)
);
CREATE INDEX swap_free_admin_fee_assess_acct_ix
    ON swap_free_admin_fee_assessments (account_id, assessed_at);
CREATE INDEX swap_free_admin_fee_assess_status_ix
    ON swap_free_admin_fee_assessments (status);

COMMIT;
