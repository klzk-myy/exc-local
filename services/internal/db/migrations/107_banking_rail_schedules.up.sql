-- 107_banking_rail_schedules
-- Phase-24 Task 24.3.20 — banking rail cut-off schedule enforcement and
-- automatic value-date roll (spec §17.16a, §24 #413).
--
-- banking_rail_schedules is the DB-driven cut-off source the Phase-11
-- static rail matrix (internal/funding RailMatrix) already anticipates:
-- "a DB-driven banking_rail_schedules table (migration 107) lands with
-- Phase-24 Task 24.3.20 and overrides these statics".
--
-- Seeds per task text (local wall-clock cut-offs; the timezone column is
-- authoritative for evaluation):
--   Fedwire USD          18:30 America/New_York (ET)
--   TARGET2 EUR          18:00 Europe/Berlin    (CET)
--   CHAPS GBP            17:00 Europe/London
--   CLS PvP initial pay-in 06:30 Europe/Berlin  (CET) — applies to all
--                          CLS-eligible currencies (currency='*')
--
-- QUEUED_FOR_NEXT_CYCLE is added to settlement_status_enum: instructions
-- generated past the rail cut-off roll their settlement_date forward and
-- sit in that status until the rolled date arrives (Task 24.3.20 step 3).

BEGIN;

CREATE TABLE banking_rail_schedules (
    id                     BIGSERIAL PRIMARY KEY,
    rail_name              VARCHAR(24)  NOT NULL,   -- FEDWIRE|TARGET2|CHAPS|CLS_PVP|...
    currency               VARCHAR(3)   NOT NULL,   -- '*' = applies to every currency
    timezone               VARCHAR(64)  NOT NULL,   -- IANA tz database name
    daily_cutoff_time      TIME         NOT NULL,   -- wall-clock in `timezone`
    settlement_cycle_days  INTEGER      NOT NULL DEFAULT 0, -- rail settlement lag (T+n)
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (rail_name, currency)
);

INSERT INTO banking_rail_schedules
    (rail_name, currency, timezone, daily_cutoff_time, settlement_cycle_days)
VALUES
    ('FEDWIRE',  'USD', 'America/New_York', '18:30', 0),
    ('TARGET2',  'EUR', 'Europe/Berlin',    '18:00', 0),
    ('CHAPS',    'GBP', 'Europe/London',    '17:00', 0),
    ('CLS_PVP',  '*',   'Europe/Berlin',    '06:30', 0);

ALTER TYPE settlement_status_enum ADD VALUE IF NOT EXISTS 'QUEUED_FOR_NEXT_CYCLE';

COMMIT;
