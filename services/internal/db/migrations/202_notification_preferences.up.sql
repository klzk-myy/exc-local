-- 202_notification_preferences.up.sql
-- Phase-12 Task 12.3.6 — per-user notification preferences.
--
-- Design ruling (one JSONB row per user, not per-(user,event,channel)
-- rows):
--   * The dispatcher's hot read is "the whole preference set for this
--     user" — a single row keeps that read one SELECT, atomically
--     consistent (a mid-PUT matrix never half-applies), and trivially
--     cacheable. ~25 per-cell rows would multiply the read and let a
--     partial update interleave with dispatch.
--   * `matrix` is {event: {channel: bool}}; absent cells resolve to the
--     service's documented defaults (email+ws on, sms+push off) so new
--     events/channels need no migration — validation lives in Go
--     (internal/notifications), the same place the event/channel
--     vocabulary is defined. Postgres enforces shape only (jsonb object).
--   * Quiet hours are plain columns (not folded into the JSONB): they
--     are a fixed, spec-pinned triple with real CHECK semantics
--     (HH24:MM), and future per-window timezone work deserves schema,
--     not opaque payload. Window is evaluated in UTC — a tz column is
--     deliberately absent until a per-user-locale requirement lands.
--
-- One row per user enforced by the PK; absent row = all defaults.

BEGIN;

CREATE TABLE notification_preferences (
    user_id        BIGINT       PRIMARY KEY REFERENCES users (id),
    matrix         JSONB        NOT NULL DEFAULT '{}'::jsonb, -- {event: {channel: enabled}}
    quiet_enabled  BOOLEAN      NOT NULL DEFAULT FALSE,
    quiet_start    TIME,                                        -- UTC HH24:MM; NULL window start = disabled
    quiet_end      TIME,                                        -- UTC HH24:MM
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT notification_preferences_matrix_obj CHECK (jsonb_typeof(matrix) = 'object'),
    CONSTRAINT notification_preferences_quiet_pair CHECK (
        (quiet_start IS NULL) = (quiet_end IS NULL)),
    CONSTRAINT notification_preferences_quiet_enabled CHECK (
        NOT quiet_enabled OR (quiet_start IS NOT NULL AND quiet_end IS NOT NULL))
);

COMMIT;
