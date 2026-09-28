-- 197_ops_status.up.sql
-- Phase-09 Task 9.3.25 — public status page + operational health exporter.
--
-- ops_status_events   append-only transition ledger: one row every time a
--                     component (or the aggregate 'system') changes state
--                     or the degradation mode changes. The uptime windows
--                     served on the status page are computed from this
--                     ledger — never fabricated.
-- ops_component_state latest observed state per component (upserted by
--                     the exporter) so a restarted exporter resumes with
--                     context instead of a blank slate.
-- ops_incidents       public-facing incident notices; the exporter posts
--                     an OPEN row when a non-Normal degradation mode
--                     activates and resolves it on recovery.
--                     postmortem_url carries the §9.3.9 artifact link —
--                     "historical post-mortems publicly accessible" is
--                     served by the public flag + this column.

BEGIN;

CREATE TABLE ops_status_events (
    id         BIGSERIAL PRIMARY KEY,
    component  TEXT        NOT NULL,            -- 'system' for the aggregate
    from_state TEXT,                            -- NULL on first observation
    to_state   TEXT        NOT NULL
               CHECK (to_state IN
                   ('operational', 'degraded_performance',
                    'partial_outage', 'major_outage', 'maintenance',
                    'unknown')),
    mode       TEXT        NOT NULL DEFAULT 'Normal',
    detail     TEXT        NOT NULL DEFAULT '',
    at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_ops_status_events_component_at
    ON ops_status_events (component, at DESC);

CREATE TABLE ops_component_state (
    component   TEXT        PRIMARY KEY,
    state       TEXT        NOT NULL,
    mode        TEXT        NOT NULL DEFAULT 'Normal',
    detail      TEXT        NOT NULL DEFAULT '',
    latency_ms  DOUBLE PRECISION NOT NULL DEFAULT 0,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ops_incidents (
    id             BIGSERIAL PRIMARY KEY,
    title          TEXT        NOT NULL,
    severity       VARCHAR(2)  NOT NULL
                   CHECK (severity IN ('P0', 'P1', 'P2', 'P3')),
    status         VARCHAR(12) NOT NULL DEFAULT 'OPEN'
                   CHECK (status IN ('OPEN', 'MONITORING', 'RESOLVED')),
    public         BOOLEAN     NOT NULL DEFAULT true,
    mode           TEXT,
    started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at    TIMESTAMPTZ,
    summary        TEXT        NOT NULL DEFAULT '',
    postmortem_url TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_ops_incidents_public_started
    ON ops_incidents (started_at DESC) WHERE public;

COMMIT;
