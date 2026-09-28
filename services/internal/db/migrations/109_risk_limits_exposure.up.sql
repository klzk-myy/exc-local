-- 109_risk_limits_exposure.up.sql
-- Phase-03 Task 3.3.5 — Risk Limits Enforcement.
-- risk_limits (migration 011 / spec §5.10) lacks the per-symbol exposure
-- columns required by spec §13.6 and by Task 3.3.5 step 3 ("per-symbol
-- limits: max notional exposure, max short exposure"). This ALTER adds
-- them without touching the shipped 011 file:
--   max_notional_exposure — per (account|tier|global) × symbol gross
--     notional cap; §13.6 default $10,000,000.
--   max_short_exposure   — per-symbol short notional cap;
--     §13.6 default $5,000,000.
--   max_account_notional — account-wide all-symbols cap; §13.6 default
--     $50,000,000 (meaningful on account-level/global rows where
--     symbol IS NULL).
-- NULL = unset/inherit at resolution time.
--
-- risk_daily_usage is the authoritative daily utilisation ledger keyed by
-- UTC calendar day: the (account_id, day) PK makes the 00:00 UTC reset
-- structural — a new UTC day starts at zero with no reset job.

BEGIN;

ALTER TABLE risk_limits
    ADD COLUMN max_notional_exposure DECIMAL(28,8),
    ADD COLUMN max_short_exposure    DECIMAL(28,8),
    ADD COLUMN max_account_notional  DECIMAL(28,8);

CREATE TABLE risk_daily_usage (
    account_id BIGINT        NOT NULL REFERENCES accounts (id),
    day        DATE          NOT NULL,               -- UTC calendar day
    volume     DECIMAL(28,8) NOT NULL DEFAULT 0,     -- traded notional today
    withdrawn  DECIMAL(28,8) NOT NULL DEFAULT 0,     -- withdrawn today
    updated_at TIMESTAMPTZ   NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, day)
);

COMMIT;
