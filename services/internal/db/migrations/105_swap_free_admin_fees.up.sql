-- 105_swap_free_admin_fees.up.sql
-- Phase-03 Task 3.3.23 (spec §5.45, §12.8a, §24 #407): Shariah-compliant
-- swap-free administrative holding-fee SCHEDULE.
--
-- Per the task text this table is the per-instrument fee schedule
-- (holding_grace_days + daily_admin_fee_usd_per_lot), NOT the per-position
-- assessment ledger that spec §5.45.3 describes under the same name — the
-- assessment rows live in swap_free_admin_fee_assessments (migration 117).
-- Recorded deviation: spec §5.45.3's column list for `swap_free_admin_fees`
-- (account_id/position_id/holding_days/admin_fee_amount/status) describes
-- the charge AUDIT table; the fee-engine schedule of Task 3.3.23 keeps the
-- canonical table name here and the audit shape moves to 117.
--
-- instrument_id NULL = global default (a per-instrument row wins over the
-- global default, mirroring swap_markup_policies resolution).

BEGIN;

CREATE TABLE swap_free_admin_fees (
    id                         BIGSERIAL PRIMARY KEY,
    instrument_id              BIGINT REFERENCES instruments (id),  -- NULL = global default
    holding_grace_days         INTEGER       NOT NULL DEFAULT 5
        CHECK (holding_grace_days >= 0),
    daily_admin_fee_usd_per_lot DECIMAL(10,4) NOT NULL
        CHECK (daily_admin_fee_usd_per_lot >= 0),
    created_at                 TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at                 TIMESTAMPTZ   NOT NULL DEFAULT now()
);

-- At most one schedule row per instrument …
CREATE UNIQUE INDEX uq_swap_free_admin_fees_instr
    ON swap_free_admin_fees (instrument_id)
    WHERE instrument_id IS NOT NULL;

-- … and exactly one global default row (constant-keyed partial index —
-- NULLs are never equal under a plain unique index).
CREATE UNIQUE INDEX uq_swap_free_admin_fees_global
    ON swap_free_admin_fees ((1))
    WHERE instrument_id IS NULL;

COMMIT;
