-- 252_option_barrier_events.up.sql
-- Phase-22 Task 22.3.5 — Barrier options knock-event log (spec §15.1,
-- §15.7 item 3, §24 #61; Phase-22 AC rows 17–21 "cash settlement at
-- expiry with barrier event logging").
--
--   option_barrier_events — one row per knock transition. A barrier
--                           touches at most once (knock-in activates /
--                           knock-out kills), enforced by
--                           UNIQUE(order_id) — a duplicate insert is a
--                           sequencing bug and fails closed. `gap`
--                           carries the §15.7 weekend-gap flag (first
--                           post-gap mark applies, flagged).
--
-- Retention (§19.12 / MiFID II RTS 6/22): >= 5 years alongside the
-- parent order record.

BEGIN;

CREATE TABLE option_barrier_events (
    id            BIGSERIAL     PRIMARY KEY,
    order_id      BIGINT        NOT NULL,           -- orders.id carrying the barrier
    instrument_id BIGINT        NOT NULL,
    barrier_type  VARCHAR(12)   NOT NULL
                  CHECK (barrier_type IN ('UP_AND_IN','UP_AND_OUT','DOWN_AND_IN','DOWN_AND_OUT')),
    barrier_level DECIMAL(20,8) NOT NULL CHECK (barrier_level > 0),
    event         VARCHAR(9)    NOT NULL CHECK (event IN ('KNOCK_IN','KNOCK_OUT')),
    mark_price    DECIMAL(20,8) NOT NULL CHECK (mark_price > 0),
    gap           BOOLEAN       NOT NULL DEFAULT false,
    observed_at   TIMESTAMPTZ   NOT NULL,           -- mark-tick timestamp, not insert time
    created_at    TIMESTAMPTZ   NOT NULL DEFAULT now(),
    UNIQUE (order_id)
);
CREATE INDEX option_barrier_events_instr_ix
    ON option_barrier_events (instrument_id, observed_at DESC);
COMMENT ON TABLE option_barrier_events IS
    'Task 22.3.5 barrier knock-event log (spec §15.1/§15.7, §24 #61): '
    'one row per knock transition on discrete mark ticks; gap flags the '
    'first post-silence mark (weekend/feed-outage); UNIQUE(order_id) '
    'enforces knock-at-most-once.';

COMMIT;
