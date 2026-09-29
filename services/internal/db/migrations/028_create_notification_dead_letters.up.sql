-- 028_create_notification_dead_letters.up.sql
-- Phase-12 Task 12.3.5 — notification delivery tracking + dead letters.
--
-- Two tables, one pipeline:
--
--   notification_deliveries   the §24 #100 delivery-tracking record —
--                             EVERY notification attempt target records
--                             channel/status/attempts/delivered_at, not
--                             only failures. One row per (notification,
--                             channel): Notify fans one user event out
--                             to each enabled channel and each leg is
--                             tracked independently.
--   notification_dead_letters the task-specified DLQ — a terminal copy
--                             of the (delivery) that exhausted
--                             max_attempts, kept as its own table so
--                             ops review queues stay small and the
--                             deliveries table can archive/partition
--                             without dragging failures along.
--
-- Why two tables instead of status='DEAD_LETTERED' alone: the task
-- pins the notification_dead_letters table name; mirroring the
-- webhooks precedent (180 — deliveries double as DLQ) would have
-- contradicted the migration note. Dead letters also land as
-- notification_deliveries.status='DEAD_LETTERED' so the per-channel
-- tracking contract holds on one table.
--
-- Status lattice: QUEUED → DELIVERED | SUPPRESSED | DEAD_LETTERED.
-- SUPPRESSED = channel disabled by user preference or recipient
-- unroutable between enqueue and dispatch (not an error, no retry).
-- QUEUED rows also carry retry bookkeeping on the Redis side
-- (notifications:pending list + notifications:retry zset) — the table
-- tracks outcomes, not scheduling.

BEGIN;

CREATE TABLE notification_deliveries (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT       NOT NULL REFERENCES users (id),
    channel       VARCHAR(8)   NOT NULL,                 -- email | sms | push | ws
    event         VARCHAR(48)  NOT NULL,                 -- deposit_confirmed | withdrawal_completed | order_filled | kyc_approved | kyc_rejected | liquidation_warning | security_alert
    payload       JSONB        NOT NULL,
    status        VARCHAR(16)  NOT NULL DEFAULT 'QUEUED', -- QUEUED | DELIVERED | SUPPRESSED | DEAD_LETTERED
    attempts      INTEGER      NOT NULL DEFAULT 0,
    max_attempts  INTEGER      NOT NULL DEFAULT 5,
    last_error    TEXT,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    delivered_at  TIMESTAMPTZ,
    CONSTRAINT notification_deliveries_channel_ck CHECK (channel IN ('email','sms','push','ws')),
    CONSTRAINT notification_deliveries_status_ck  CHECK (status IN ('QUEUED','DELIVERED','SUPPRESSED','DEAD_LETTERED')),
    CONSTRAINT notification_deliveries_attempts_nonneg CHECK (attempts >= 0),
    CONSTRAINT notification_deliveries_max_pos         CHECK (max_attempts > 0)
);

-- Per-user recent-history read (admin/support views, tests).
CREATE INDEX idx_notification_deliveries_user ON notification_deliveries (user_id, id DESC);
-- Open-work sweep + stuck-QUEUE audits.
CREATE INDEX idx_notification_deliveries_queued ON notification_deliveries (id)
    WHERE status = 'QUEUED';
-- Dead-letter review surface.
CREATE INDEX idx_notification_deliveries_dead ON notification_deliveries (user_id)
    WHERE status = 'DEAD_LETTERED';

CREATE TABLE notification_dead_letters (
    id           BIGSERIAL PRIMARY KEY,
    delivery_id  BIGINT       NOT NULL REFERENCES notification_deliveries (id),
    user_id      BIGINT       NOT NULL REFERENCES users (id),
    channel      VARCHAR(8)   NOT NULL,
    event        VARCHAR(48)  NOT NULL,
    payload      JSONB        NOT NULL,
    attempts     INTEGER      NOT NULL,
    last_error   TEXT,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT notification_dead_letters_channel_ck CHECK (channel IN ('email','sms','push','ws'))
);

CREATE INDEX idx_notification_dead_letters_user ON notification_dead_letters (user_id, id DESC);

COMMIT;
