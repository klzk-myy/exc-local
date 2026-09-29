-- 221_market_schedule_overrides.up.sql
-- Phase-15 Task 15.3.4 — admin-editable market-schedule overrides
-- (spec §1 24/5 canonical window; §24 #217/#343).
--
-- PostgreSQL is the system of record for schedule overrides; the Go
-- MarketScheduleService republishes the merged schedule to the Redis
-- `market:hours` key on every committed change and at gateway boot.
-- The C++ PreTradeChecker consumes `market:hours` — Redis is a
-- read-through projection, never the source of truth.
--
-- One row per UTC civil date:
--   closed = true                       → full-day holiday (market closed
--                                         for the whole UTC date)
--   closed = false + open/close set     → shortened session inside the
--                                         canonical weekly window
--     (e.g. Christmas Eve early close)
--
-- A partial window must lie strictly inside the canonical day session
-- (open < close, both HH:MM UTC). `closed` rows must not carry times —
-- ambiguous rows would silently mis-shape the engine's hours gate, so
-- the CHECKs fail closed.

BEGIN;

CREATE TABLE market_schedule_overrides (
    override_id   BIGSERIAL PRIMARY KEY,
    override_date DATE         NOT NULL,              -- UTC civil date
    closed        BOOLEAN      NOT NULL DEFAULT false,
    open_utc      TIME,                               -- HH:MM UTC, partial open
    close_utc     TIME,                               -- HH:MM UTC, partial close
    reason        TEXT         NOT NULL,
    created_by    BIGINT       NOT NULL,              -- admin user id (audit)
    updated_by    BIGINT,                             -- last mutating admin
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT market_schedule_overrides_date_uq UNIQUE (override_date),
    CONSTRAINT mso_window_ck CHECK (
        closed OR (open_utc IS NOT NULL AND close_utc IS NOT NULL
                   AND open_utc < close_utc)
    ),
    CONSTRAINT mso_closed_no_times_ck CHECK (
        NOT closed OR (open_utc IS NULL AND close_utc IS NULL)
    ),
    CONSTRAINT mso_reason_nonempty_ck CHECK (length(btrim(reason)) > 0)
);

-- Override lookup is always by date; the unique constraint already
-- covers it, so no extra index is needed.

COMMIT;
