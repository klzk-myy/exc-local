-- 172_maintenance_windows.up.sql
-- Phase-05 Task 5.3.14 — maintenance calendar.
--
-- maintenance_windows: scheduled work windows surfaced publicly at
-- GET /api/v1/maintenance/schedule. CANCELLED/COMPLETED windows stay on
-- record (status page history + ops audit); the public schedule endpoint
-- returns only SCHEDULED/IN_PROGRESS windows whose ends_at is still ahead.

BEGIN;

CREATE TYPE maintenance_scope_enum AS ENUM (
    'FULL_VENUE', 'GATEWAY', 'MARKET_DATA', 'SETTLEMENT', 'FUNDING', 'INSTRUMENT'
);
CREATE TYPE maintenance_status_enum AS ENUM (
    'SCHEDULED', 'IN_PROGRESS', 'COMPLETED', 'CANCELLED'
);

CREATE TABLE maintenance_windows (
    id          BIGSERIAL PRIMARY KEY,
    title       VARCHAR(200)             NOT NULL,
    description TEXT,
    scope       maintenance_scope_enum   NOT NULL DEFAULT 'FULL_VENUE',
    symbols     TEXT[],                                -- populated when scope=INSTRUMENT
    status      maintenance_status_enum  NOT NULL DEFAULT 'SCHEDULED',
    starts_at   TIMESTAMPTZ              NOT NULL,
    ends_at     TIMESTAMPTZ              NOT NULL,
    created_by  VARCHAR(128),                          -- admin subject
    created_at  TIMESTAMPTZ              NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ              NOT NULL DEFAULT now(),
    CONSTRAINT maintenance_window_bounds CHECK (ends_at > starts_at)
);

CREATE INDEX idx_maintenance_windows_upcoming
    ON maintenance_windows (starts_at)
    WHERE status IN ('SCHEDULED', 'IN_PROGRESS');

COMMIT;
