-- 118_positions_rollover_columns.up.sql
-- Phase-03 Tasks 3.3.7 / 3.3.22 / 3.3.23 (spec §17.4, §17.4a, §5.45,
-- §24 #122/#406/#407): columns and bookkeeping the Tom-Next rollover
-- service requires.
--
-- 1. positions.opened_at — task text for 3.3.23 inspects position.opened_at
--    to measure holding duration vs the swap-free grace period. Migration
--    014 never created it (positions carried only updated_at); DEFAULT now()
--    backfills existing rows.
-- 2. positions.value_date — the current spot value date the roll advances
--    to the next mutual business day (Task 3.3.7 step 4). NULL = not yet
--    rolled; the service derives the initial value date from the
--    instrument's settlement cycle at first roll.
-- 3. swap_accrual_records.roll_date + partial unique index — makes the
--    per-position accrual audit row idempotent per roll date (retried runs
--    upsert, never double-record).
-- 4. rollover_runs — per-day run bookkeeping: idempotent run dedup
--    (roll_date UNIQUE), resume-safe status, and the audit counters the
--    ops/finance report needs before Asia session open.

BEGIN;

ALTER TABLE positions
    ADD COLUMN IF NOT EXISTS opened_at TIMESTAMPTZ NOT NULL DEFAULT now();

ALTER TABLE positions
    ADD COLUMN IF NOT EXISTS value_date DATE;

ALTER TABLE swap_accrual_records
    ADD COLUMN IF NOT EXISTS roll_date DATE;

CREATE UNIQUE INDEX IF NOT EXISTS uq_swap_accrual_records_roll
    ON swap_accrual_records (position_id, roll_date)
    WHERE roll_date IS NOT NULL;

CREATE TABLE rollover_runs (
    id                BIGSERIAL PRIMARY KEY,
    roll_date         DATE         NOT NULL UNIQUE,       -- ET (New York) trading date
    status            VARCHAR(16)  NOT NULL DEFAULT 'RUNNING'
        CHECK (status IN ('RUNNING', 'COMPLETED', 'FAILED')),
    started_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    completed_at      TIMESTAMPTZ,
    positions_scanned INTEGER      NOT NULL DEFAULT 0,
    positions_rolled  INTEGER      NOT NULL DEFAULT 0,
    positions_skipped INTEGER      NOT NULL DEFAULT 0,
    fees_assessed     INTEGER      NOT NULL DEFAULT 0,
    error_text        TEXT
);
CREATE INDEX rollover_runs_status_ix ON rollover_runs (status, roll_date);

COMMIT;
