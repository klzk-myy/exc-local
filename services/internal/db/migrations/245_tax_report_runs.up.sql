-- 245_tax_report_runs.up.sql
-- Phase-21 Task 21.3.22 — automated tax reporting: CRS / FATCA
-- authority submissions (spec §14.11, §19.12). Reuses the Task
-- 14.3.x tax_self_certifications PII store (migrations 205 + 210,
-- AES-256-GCM sealed) as the identity source — this table holds only
-- run-level metadata, never reportable PII.
--
--   tax_report_runs — one row per generated artifact version:
--                     (regime, report_year, jurisdiction, version) is
--                     the natural identity; a regeneration bumps
--                     version and supersedes the prior non-submitted
--                     run. Lifecycle mirrors the SAR register:
--                     DRAFT → UNDER_REVIEW → APPROVED → SUBMITTED with
--                     dual control at APPROVED (approver ≠ creator and
--                     ≠ reviewer). FAILED/REJECTED are terminal for the
--                     version; SUPERSEDED marks a replaced draft.
--
-- artifact_ref points at the sealed report object (WORM store); the
-- sha256 column pins the artifact bytes so a regen or tamper is
-- detectable without reading the blob.

BEGIN;

CREATE TABLE tax_report_runs (
    id               BIGSERIAL   PRIMARY KEY,
    regime           VARCHAR(8)  NOT NULL CHECK (regime IN ('CRS', 'FATCA', 'CLIENT_STATEMENT')),
    report_year      INTEGER     NOT NULL CHECK (report_year BETWEEN 2000 AND 2100),
    jurisdiction     VARCHAR(8)  NOT NULL DEFAULT '',   -- CRS: reportable jurisdiction; FATCA: 'US'
    version          INTEGER     NOT NULL DEFAULT 1,
    status           VARCHAR(16) NOT NULL DEFAULT 'DRAFT' CHECK (status IN (
        'DRAFT', 'UNDER_REVIEW', 'APPROVED', 'SUBMITTED',
        'FAILED', 'REJECTED', 'SUPERSEDED')),
    artifact_ref     VARCHAR(512),
    artifact         BYTEA,                              -- canonical XML bytes (the stored artifact copy; S3 mirror optional)
    sha256           CHAR(64),
    account_count    INTEGER     NOT NULL DEFAULT 0 CHECK (account_count >= 0),
    detail           JSONB       NOT NULL DEFAULT '{}', -- nil-report flags, aggregate stats
    created_by       BIGINT      NOT NULL,
    reviewed_by      BIGINT,
    reviewed_at      TIMESTAMPTZ,
    approved_by      BIGINT,
    approved_at      TIMESTAMPTZ,
    rejected_by      BIGINT,
    rejected_at      TIMESTAMPTZ,
    rejection_reason TEXT,
    submitted_by     BIGINT,
    submitted_at     TIMESTAMPTZ,
    submission_ref   VARCHAR(128),                      -- authority receipt reference
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (regime, report_year, jurisdiction, version)
);

CREATE INDEX ix_tax_report_runs_status
    ON tax_report_runs (status, report_year DESC, id DESC);

COMMIT;
