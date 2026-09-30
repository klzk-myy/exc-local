-- 273_dora_incidents
-- Phase-09 Tasks 9.3.15 + 9.3.18 — DORA ICT incident lifecycle store.
--
-- `incidents` is the internal incident record already probed by the
-- fleet deploy gates (severity P0/P1 open-count interlock) and the
-- admin CEO/board packs (created_at window counts, rca_status OPEN).
-- `incident_regulator_reports` carries the DORA initial/intermediate/
-- final regulator-report deadlines (spec §19.8: 4h / 72h / 1-month,
-- anchored on detected_at); `incident_remediations` carries the
-- lessons-learned / corrective-action items with board/risk acceptance.
-- The material-incident closure gate in internal/operations/dora
-- refuses CLOSED while required reports are unsubmitted (overdue or
-- pending) or remediations are unresolved/unaccepted (spec §19.5).

BEGIN;

CREATE TABLE incidents (
    id              BIGSERIAL PRIMARY KEY,
    ref             TEXT        NOT NULL UNIQUE,        -- INC-YYYYMMDD-####
    severity        VARCHAR(2)  NOT NULL
                    CHECK (severity IN ('P0', 'P1', 'P2', 'P3')),
    material        BOOLEAN     NOT NULL DEFAULT false, -- DORA-reportable (P0 always; P1 by classification)
    status          VARCHAR(12) NOT NULL DEFAULT 'OPEN'
                    CHECK (status IN ('OPEN', 'ACKNOWLEDGED', 'MITIGATED', 'RESOLVED', 'CLOSED')),
    title           TEXT        NOT NULL,
    classification  JSONB       NOT NULL DEFAULT '{}'::jsonb, -- impact/clients/geography/data-loss/services (§19.5 item 2)
    rca_status      VARCHAR(12) NOT NULL DEFAULT 'OPEN'
                    CHECK (rca_status IN ('OPEN', 'DRAFT', 'COMPLETE')),
    root_cause      TEXT        NOT NULL DEFAULT '',
    lessons_learned TEXT        NOT NULL DEFAULT '',
    detected_at     TIMESTAMPTZ NOT NULL DEFAULT now(),   -- DORA deadline anchor
    acknowledged_at TIMESTAMPTZ,
    mitigated_at    TIMESTAMPTZ,
    resolved_at     TIMESTAMPTZ,
    closed_at       TIMESTAMPTZ,
    closed_by       BIGINT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX incidents_open_ix ON incidents (severity) WHERE status <> 'CLOSED';

CREATE TABLE incident_regulator_reports (
    id           BIGSERIAL PRIMARY KEY,
    incident_id  BIGINT       NOT NULL REFERENCES incidents (id),
    kind         VARCHAR(12)  NOT NULL CHECK (kind IN ('INITIAL', 'INTERMEDIATE', 'FINAL')),
    regulator    TEXT         NOT NULL DEFAULT '',
    due_at       TIMESTAMPTZ  NOT NULL,
    submitted_at TIMESTAMPTZ,
    submitted_by BIGINT,
    approved_at  TIMESTAMPTZ,
    approved_by  BIGINT,
    evidence_ref TEXT         NOT NULL DEFAULT '',         -- immutable evidence object key
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (incident_id, kind)
);
CREATE INDEX incident_regulator_reports_open_ix
    ON incident_regulator_reports (due_at) WHERE submitted_at IS NULL;

CREATE TABLE incident_remediations (
    id          BIGSERIAL PRIMARY KEY,
    incident_id BIGINT       NOT NULL REFERENCES incidents (id),
    action      TEXT         NOT NULL,
    owner       TEXT         NOT NULL,
    due_at      TIMESTAMPTZ  NOT NULL,
    status      VARCHAR(12)  NOT NULL DEFAULT 'OPEN'
                CHECK (status IN ('OPEN', 'IN_PROGRESS', 'RESOLVED', 'ACCEPTED')),
    resolved_at TIMESTAMPTZ,
    accepted_by BIGINT,                                    -- board/risk acceptance principal
    accepted_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX incident_remediations_open_ix
    ON incident_remediations (incident_id) WHERE status <> 'ACCEPTED';

COMMIT;
