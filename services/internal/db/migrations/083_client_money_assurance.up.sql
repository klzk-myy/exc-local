-- 083_client_money_assurance.up.sql
-- Phase-24 Task 24.3.18 — independent client-money assurance & segregation
-- certification (spec §17.13.2, §24 #330).

BEGIN;

-- Engagement register: one row per independent safeguarding audit.
-- independence_* fields record the contractual-independence fact and its
-- annual review (spec §17.13.2 item 5).
CREATE TABLE client_money_audits (
    id               BIGSERIAL PRIMARY KEY,
    engagement_year  INT NOT NULL,
    auditor_firm     VARCHAR(128) NOT NULL,
    scope            TEXT NOT NULL,
    period_start     DATE NOT NULL,
    period_end       DATE NOT NULL,
    status           VARCHAR(12) NOT NULL DEFAULT 'SCHEDULED'
        CHECK (status IN ('SCHEDULED','FIELDWORK','DRAFT','ISSUED')),
    independence_confirmed  BOOLEAN NOT NULL DEFAULT FALSE,
    independence_statement  TEXT,             -- recorded engagement field
    independence_reviewed_at TIMESTAMPTZ,     -- annual review stamp
    evidence_requests JSONB NOT NULL DEFAULT '[]',  -- evidence-request log
    findings          JSONB NOT NULL DEFAULT '[]',
    remediation_tickets JSONB NOT NULL DEFAULT '[]',
    created_by       BIGINT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (period_end >= period_start)
);
CREATE INDEX cmaudit_status_ix ON client_money_audits (status, engagement_year);

-- Evidence packs — assembled ONLY from the system of record (segregation
-- calcs + sign-offs, bank reconciliation, top-up log, GL lines, §17.11
-- Merkle PoR roots), never hand-assembled. sha256 over the canonical JSON.
CREATE TABLE client_money_evidence_packs (
    id           BIGSERIAL PRIMARY KEY,
    audit_id     BIGINT NOT NULL REFERENCES client_money_audits (id),
    period_start DATE NOT NULL,
    period_end   DATE NOT NULL,
    pack         JSONB NOT NULL,
    pack_sha256  CHAR(64) NOT NULL,
    assembled_by BIGINT NOT NULL,
    assembled_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX cmep_audit_ix ON client_money_evidence_packs (audit_id, assembled_at DESC);

-- Segregation certifications (spec §17.13.2 item 3): the artefact satisfying
-- §17.9's legal segregation obligation. An expired or missing certification
-- for a covered period blocks the Phase-24 → production release gate.
-- Issuance is dual-controlled (issued_by ≠ approved_by).
CREATE TABLE segregation_certifications (
    id              BIGSERIAL PRIMARY KEY,
    audit_id        BIGINT NOT NULL REFERENCES client_money_audits (id),
    evidence_pack_id BIGINT REFERENCES client_money_evidence_packs (id),
    period_start    DATE NOT NULL,
    period_end      DATE NOT NULL,
    statement       TEXT NOT NULL,
    signatories     JSONB NOT NULL,
    pack_sha256     CHAR(64) NOT NULL,
    issued_by       BIGINT NOT NULL,
    approved_by     BIGINT NOT NULL,
    published_until TIMESTAMPTZ NOT NULL,
    status          VARCHAR(10) NOT NULL DEFAULT 'ISSUED'
        CHECK (status IN ('ISSUED','EXPIRED','REVOKED')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (issued_by <> approved_by)
);

-- EXTERNAL_AUDITOR access grants (spec §17.13.2 item 4): read-only,
-- time-bounded, dual-controlled, every access logged. The role resolves via
-- the EXTERNAL_AUDITOR principal_role_system (admin.SystemExternalAuditor).
CREATE TABLE external_auditor_grants (
    id              BIGSERIAL PRIMARY KEY,
    audit_id        BIGINT NOT NULL REFERENCES client_money_audits (id),
    auditor_ref     VARCHAR(128) NOT NULL,      -- external identity / provisioned user ref
    granted_by      BIGINT NOT NULL,
    approved_by     BIGINT NOT NULL,            -- dual control (≠ granted_by)
    valid_from      TIMESTAMPTZ NOT NULL,
    valid_until     TIMESTAMPTZ NOT NULL,       -- time-bounded
    status          VARCHAR(10) NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE','EXPIRED','REVOKED')),
    access_log      JSONB NOT NULL DEFAULT '[]', -- every read stamped here
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (granted_by <> approved_by),
    CHECK (valid_until > valid_from)
);
CREATE INDEX eag_audit_ix ON external_auditor_grants (audit_id, status);

COMMIT;
