-- 060_compliance_assessments.up.sql
-- Phase-21 Task 21.3.17 — FX Global Code 55-principle self-assessment &
-- annual review engine (spec §5.37/§14.6; §24 #182; §27.1 FX Global Code
-- matrix row → FX_GLOBAL_CODE_NON_COMPLIANT audit warning).
--
--   compliance_assessment_runs — one annual review run per (framework,
--                                period): the container every principle
--                                verdict, the aggregate score and the
--                                executive sign-off hang off.
--   compliance_assessments     — the spec §5.37 per-(run, principle)
--                                adherence matrix row. automated=TRUE
--                                marks programmatically verified
--                                controls (P9 firm liquidity, P10
--                                timestamp fidelity, P17 pre-hedging
--                                prohibition, P50 PvP settlement);
--                                PENDING rows await officer assessment
--                                (a run cannot COMPLETE or sign with any
--                                PENDING verdict — fail-closed).
--   fx_gc_statements           — the Statement of Commitment artefact:
--                                generated per run, sha256-pinned body,
--                                executive sign-off + public-register
--                                publication flag.
--
-- Retention (§19.12): regulatory records — no purge below the 7-year
-- compliance horizon.

BEGIN;

CREATE TABLE compliance_assessment_runs (
    id                  BIGSERIAL    PRIMARY KEY,
    framework           VARCHAR(32)  NOT NULL DEFAULT 'FX_GLOBAL_CODE'
                        CHECK (framework IN ('FX_GLOBAL_CODE')),
    code_version        VARCHAR(32)  NOT NULL,          -- Global Code edition, e.g. 'DEC_2024'
    period              VARCHAR(16)  NOT NULL,          -- review period, e.g. 'FY2026'
    status              VARCHAR(16)  NOT NULL DEFAULT 'RUNNING'
                        CHECK (status IN ('RUNNING','COMPLETED','SIGNED')),
    started_by          BIGINT       NOT NULL,          -- Compliance Officer user id
    principles_total    INTEGER      NOT NULL DEFAULT 55,
    principles_adherent INTEGER      NOT NULL DEFAULT 0,
    principles_partial  INTEGER      NOT NULL DEFAULT 0,
    principles_non      INTEGER      NOT NULL DEFAULT 0,
    principles_pending  INTEGER      NOT NULL DEFAULT 0,
    score               NUMERIC(5,2),                   -- ADHERENT share %, set at completion
    signed_by           BIGINT,                         -- executive/CCO sign-off
    signed_at           TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (framework, period)                          -- one review run per period
);

-- compliance_assessments — spec §5.37 column set (+ run linkage,
-- automated-verification flag, evidence JSONB and remediation ticket
-- ref; additive deviations recorded for §27).
CREATE TABLE compliance_assessments (
    id               BIGSERIAL    PRIMARY KEY,
    run_id           BIGINT       NOT NULL
                     REFERENCES compliance_assessment_runs (id),
    framework        VARCHAR(32)  NOT NULL DEFAULT 'FX_GLOBAL_CODE',
    assessment_date  DATE         NOT NULL,
    principle_id     INTEGER      NOT NULL CHECK (principle_id BETWEEN 1 AND 55),
    theme            VARCHAR(64)  NOT NULL,             -- one of the 6 §14.6 themes
    adherence_status VARCHAR(16)  NOT NULL DEFAULT 'PENDING'
                     CHECK (adherence_status IN
                         ('ADHERENT','PARTIAL','NON_ADHERENT','PENDING')),
    automated        BOOLEAN      NOT NULL DEFAULT FALSE, -- engine-verified vs officer-assessed
    evidence_summary TEXT,
    evidence         JSONB        NOT NULL DEFAULT '{}', -- probe outputs / citations
    remediation_ref  VARCHAR(128),                       -- remediation ticket for PARTIAL/NON_ADHERENT
    assessor_id      BIGINT,                             -- NULL = automated engine verdict
    approved_by_cco  BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (run_id, principle_id)
);

CREATE INDEX ix_compliance_assessments_status
    ON compliance_assessments (adherence_status, principle_id);

-- fx_gc_statements — Statement of Commitment history (spec §14.6:
-- public register publication + FX Global Code Disclosure Cover Sheet).
CREATE TABLE fx_gc_statements (
    id             BIGSERIAL    PRIMARY KEY,
    run_id         BIGINT       NOT NULL UNIQUE
                   REFERENCES compliance_assessment_runs (id),
    code_version   VARCHAR(32)  NOT NULL,
    period         VARCHAR(16)  NOT NULL,
    body           TEXT         NOT NULL,               -- generated commitment text + cover sheet
    body_sha256    CHAR(64)     NOT NULL,               -- WORM-grade integrity pin
    signed_by      BIGINT,                              -- executive signatory
    signed_at      TIMESTAMPTZ,
    published      BOOLEAN      NOT NULL DEFAULT FALSE, -- public-register publication flag
    published_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX ix_fx_gc_statements_run ON fx_gc_statements (run_id);

COMMIT;
