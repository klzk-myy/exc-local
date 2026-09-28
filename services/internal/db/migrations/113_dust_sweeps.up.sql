-- 113_dust_sweeps.up.sql
-- Phase-03 Task 3.3.20 — Dust-Balance Conversion sweep ledger
-- (spec §5.21b, §24 #364).
--
-- dust_sweeps records one conversion attempt per (account, currency, UTC
-- day). The UNIQUE constraint is the daily rate limit AND the idempotency
-- anchor: a claimed-but-unposted sweep carries journal_entry_id NULL so a
-- crashed sweep can be released and retried; a posted sweep replays its
-- recorded amounts instead of converting again.

BEGIN;

CREATE TABLE dust_sweeps (
    id               BIGSERIAL PRIMARY KEY,
    account_id       BIGINT        NOT NULL REFERENCES accounts (id),
    currency         VARCHAR(3)    NOT NULL,            -- dust currency converted
    base_currency    VARCHAR(3)    NOT NULL,            -- account base credited
    sweep_date       DATE          NOT NULL,            -- UTC calendar day
    instrument_id    BIGINT        REFERENCES instruments (id),
    dust_amount      DECIMAL(28,8) NOT NULL,            -- dust-ccy amount debited
    credited_amount  DECIMAL(28,8) NOT NULL,            -- base-ccy amount credited (mid − spread)
    mid_rate         DECIMAL(28,12) NOT NULL,           -- mark mid, base per 1 dust unit
    spread_bps       DECIMAL(9,4)  NOT NULL,            -- disclosed conversion spread applied
    journal_entry_id BIGINT        REFERENCES journal_entries (id),  -- NULL until posted
    created_at       TIMESTAMPTZ   NOT NULL DEFAULT now(),
    UNIQUE (account_id, currency, sweep_date)           -- one sweep per ccy per day per account
);

CREATE INDEX dust_sweeps_account_ix ON dust_sweeps (account_id, sweep_date);

COMMIT;
