-- 171_announcements.up.sql
-- Phase-05 Task 5.3.14 — announcements & status-page records.
--
-- announcements: operator-published client-visible notices. Public reads see
-- only PUBLISHED rows inside their publish/expiry window; DRAFT/RETRACTED
-- rows stay admin-visible for the audit trail (DELETE retracts, never
-- destroys — announcements feed client disclosure records).

BEGIN;

CREATE TYPE announcement_category_enum AS ENUM (
    'GENERAL', 'MAINTENANCE', 'INCIDENT', 'PRODUCT', 'PROMOTION'
);
CREATE TYPE announcement_status_enum AS ENUM (
    'DRAFT', 'PUBLISHED', 'EXPIRED', 'RETRACTED'
);

CREATE TABLE announcements (
    id          BIGSERIAL PRIMARY KEY,
    title       VARCHAR(200)            NOT NULL,
    body        TEXT                    NOT NULL,
    category    announcement_category_enum NOT NULL DEFAULT 'GENERAL',
    status      announcement_status_enum   NOT NULL DEFAULT 'PUBLISHED',
    publish_at  TIMESTAMPTZ             NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ,                          -- NULL = no expiry
    created_by  VARCHAR(128),                         -- admin subject
    created_at  TIMESTAMPTZ             NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ             NOT NULL DEFAULT now()
);

-- Public list is "published and inside its window, newest first".
CREATE INDEX idx_announcements_public
    ON announcements (publish_at DESC)
    WHERE status = 'PUBLISHED';

COMMIT;
