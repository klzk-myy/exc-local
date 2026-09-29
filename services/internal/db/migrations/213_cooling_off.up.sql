-- Migration 213 — Responsible-trading cooling-off / self-exclusion
-- periods (Phase-14 Task 14.3.11).
--
-- Replaces the phase-plan placeholder "migration 070" — 070 is already
-- occupied by 070_users_anti_phishing_code. Duration is an interval
-- CHECK-constrained to the four regulated choices; acknowledgement is
-- stamped at activation. The table has NO cancelled/cleared column and
-- no mutator exists anywhere in the codebase — a period is strictly
-- irrevocable for its full duration (spec §14.9 / Task 14.3.11).

BEGIN;

CREATE TABLE cooling_off_periods (
    id              BIGSERIAL   PRIMARY KEY,
    user_id         BIGINT      NOT NULL REFERENCES users(id),
    duration        INTERVAL    NOT NULL CHECK (duration IN (
                        interval '1 day',
                        interval '3 days',
                        interval '7 days',
                        interval '30 days')),
    started_at      TIMESTAMPTZ NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    acknowledged_at TIMESTAMPTZ NOT NULL,   -- explicit client acknowledgement stamp
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- expires_at is always started_at + duration — enforced by the service
-- at insert (timestamptz+interval is STABLE so it cannot be a CHECK;
-- and the period is insert-only, so the arithmetic is unforgeable).
-- Exactly one live window per user at a time: overlap is rejected in
-- the service before insert.
CREATE INDEX idx_cooling_off_user    ON cooling_off_periods (user_id);
CREATE INDEX idx_cooling_off_expires ON cooling_off_periods (expires_at);

COMMENT ON TABLE cooling_off_periods IS
    'Phase-14 Task 14.3.11 — self-exclusion windows. Insert-only by '
    'contract: no UPDATE/DELETE path exists, no admin/support '
    'cancellation, no shortening. While expires_at > now() the '
    'order-admission gate rejects leveraged entry with '
    'COOLING_OFF_ACTIVE; spot conversions and withdrawals stay open.';

COMMIT;
