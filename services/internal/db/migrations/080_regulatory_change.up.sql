-- 080_regulatory_change.up.sql
-- Phase-21 Task 21.3.25 — Regulatory Change Monitoring & Impact
-- Assessment (spec §14.10.2; §24 #328; §27.1 Regulatory Change Horizon
-- Scanning matrix → RULEBOOK_VERSION_STALE P1 /
-- REGULATORY_DEADLINE_APPROACHING). The task text/AC page the
-- effective-within-90-days condition at P1 to the Compliance Officer —
-- stricter than the matrix's P2 floor; deviation noted for §27.
--
--   regulatory_changes                — the watch register: authority /
--                                       instrument / title / dates /
--                                       owner / lifecycle status
--                                       (TRACKED → TRIAGED → SCOPED →
--                                       IMPLEMENTED → CLOSED).
--                                       triage_due_at = published_at +
--                                       10 business days (service
--                                       computes the calendar math —
--                                       the column is the stored
--                                       deadline the SLA sweep probes).
--                                       spec_criteria_refs lists the
--                                       §24 criteria a change alters —
--                                       IMPLEMENTED is refused while
--                                       matrix_update_ref is NULL
--                                       (spec §14.10.2 item 4: the
--                                       traceability row update ships
--                                       in the same change set).
--   regulatory_change_impacts         — per-(change, kind, ref) impact
--                                       items: spec sections, phase
--                                       plans, migrations, data fields
--                                       and reporting endpoints, each
--                                       carrying owner / effort / due /
--                                       status. IMPLEMENTED requires
--                                       every recorded item DONE.
--   regulatory_change_correspondence  — regulator information holds and
--                                       requests linked to the same
--                                       change record (spec §14.10.2
--                                       item 5) — an emergency rule
--                                       change and its impact scope
--                                       share one record.
--
-- Retention (§19.12): governance records — no purge below the 7-year
-- compliance horizon.

BEGIN;

CREATE TABLE regulatory_changes (
    change_id           BIGSERIAL    PRIMARY KEY,
    authority           VARCHAR(32)  NOT NULL CHECK (authority IN (
        'ESMA','FCA','CFTC','FINRA','FATF','SEC','NFA','ECB','BOE','JFSA',
        'ASIC','MAS','BAFIN','FINMA','OTHER')),
    instrument          VARCHAR(128) NOT NULL,          -- directive/RTS/rule identifier
    title               VARCHAR(255) NOT NULL,
    published_at        TIMESTAMPTZ  NOT NULL,
    effective_at        TIMESTAMPTZ,
    source_url          VARCHAR(512),
    owner               BIGINT,                         -- responsible officer user id
    status              VARCHAR(16)  NOT NULL DEFAULT 'TRACKED'
                        CHECK (status IN
                            ('TRACKED','TRIAGED','SCOPED','IMPLEMENTED','CLOSED')),
    triage_due_at       TIMESTAMPTZ  NOT NULL,          -- published_at + 10 business days
    triaged_at          TIMESTAMPTZ,
    spec_criteria_refs  JSONB        NOT NULL DEFAULT '[]', -- §24 criteria ids altered
    matrix_update_ref   VARCHAR(128),                   -- traceability-matrix row update ref
    notes               TEXT,
    created_by          BIGINT       NOT NULL,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Untriaged dashboard queue (oldest deadline first) + near-deadline
-- effective-date probe for the REGULATORY_DEADLINE_APPROACHING sweep.
CREATE INDEX ix_reg_changes_triage
    ON regulatory_changes (triage_due_at) WHERE status = 'TRACKED';
CREATE INDEX ix_reg_changes_effective
    ON regulatory_changes (effective_at)
    WHERE status IN ('TRACKED','TRIAGED','SCOPED');
CREATE INDEX ix_reg_changes_status
    ON regulatory_changes (status, published_at DESC);
-- Idempotent register replays: the natural watch key dedups NATS/portal
-- redelivery (ON CONFLICT DO NOTHING in the service).
CREATE UNIQUE INDEX uq_reg_changes_natural
    ON regulatory_changes (authority, instrument, title, published_at);

CREATE TABLE regulatory_change_impacts (
    id               BIGSERIAL    PRIMARY KEY,
    change_id        BIGINT       NOT NULL
                     REFERENCES regulatory_changes (change_id),
    kind             VARCHAR(24)  NOT NULL CHECK (kind IN (
        'SPEC_SECTION','PHASE_TASK','MIGRATION','DATA_FIELD','ENDPOINT',
        'NO_IMPACT')),
    ref              VARCHAR(255) NOT NULL,             -- '§14.1', 'Phase-21 Task 21.3.25', '080', ...
    owner            BIGINT,                            -- implementer user id
    effort_estimate  VARCHAR(64),                       -- e.g. '3d', 'S|M|L'
    due_at           TIMESTAMPTZ,
    status           VARCHAR(16)  NOT NULL DEFAULT 'OPEN'
                     CHECK (status IN ('OPEN','DONE')),
    completed_at     TIMESTAMPTZ,
    recorded_by      BIGINT       NOT NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (change_id, kind, ref)
);

CREATE INDEX ix_reg_impacts_open
    ON regulatory_change_impacts (change_id) WHERE status = 'OPEN';

CREATE TABLE regulatory_change_correspondence (
    id           BIGSERIAL    PRIMARY KEY,
    change_id    BIGINT       NOT NULL
                 REFERENCES regulatory_changes (change_id),
    kind         VARCHAR(16)  NOT NULL CHECK (kind IN ('INFO_HOLD','INFO_REQUEST')),
    summary      TEXT         NOT NULL,
    received_at  TIMESTAMPTZ  NOT NULL,
    due_at       TIMESTAMPTZ,                           -- regulator response deadline
    recorded_by  BIGINT       NOT NULL,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX ix_reg_correspondence_change
    ON regulatory_change_correspondence (change_id, received_at DESC);

COMMIT;
