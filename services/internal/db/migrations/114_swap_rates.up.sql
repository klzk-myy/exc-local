-- 114_swap_rates.up.sql
-- Phase-03 Task 3.3.11 (spec §17.4, §24 #221): effective-dated Tom-Next
-- swap-point store. The swap rate engine ingests daily feeds (Refinitiv /
-- Bloomberg via the scheduled SwapRateFeed pull) into this table; the
-- rollover engine reads the rate effective for each roll date. Markup legs
-- and accrual audit rows live in migration 088 (swap_markup_policies /
-- swap_accrual_records); §17.15 history is served from this table + the
-- accrual journal.

BEGIN;

CREATE TABLE swap_rates (
    id               BIGSERIAL PRIMARY KEY,
    instrument_id    BIGINT       NOT NULL REFERENCES instruments (id),
    effective_date   DATE         NOT NULL,
    long_swap_points  DECIMAL(28,8) NOT NULL,  -- points/day applied to LONG positions
    short_swap_points DECIMAL(28,8) NOT NULL,  -- points/day applied to SHORT positions
    source           VARCHAR(32)  NOT NULL DEFAULT 'MANUAL', -- REFINITIV | BLOOMBERG | FILE | HTTP | MANUAL
    ingested_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (instrument_id, effective_date)
);

-- Latest-rate and history lookups are (instrument, date desc) scans.
CREATE INDEX swap_rates_lookup_ix ON swap_rates (instrument_id, effective_date DESC);

COMMIT;
