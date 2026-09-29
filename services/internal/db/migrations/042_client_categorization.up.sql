-- 042_client_categorization.up.sql
-- Phase-14 Task 14.3.7 — MiFID II client categorization & appropriateness
-- (spec §5.2 accounts.client_category, §14.2, §24 #132).
--
-- Three objects:
--   client_category_enum / accounts.client_category — the regulatory
--     category axis, orthogonal to kyc_tier (identity). RETAIL is the
--     onboarding default; PROFESSIONAL / ELIGIBLE_COUNTERPARTY require a
--     Compliance-Officer workflow with documented MiFID II Annex II
--     eligibility evidence (audit-logged through admin_audit_log, never
--     client-writable). The migration-203 convention resolves an
--     approved INSTITUTIONAL submission to kyc_tier 'T2' +
--     client_category 'ELIGIBLE_COUNTERPARTY'.
--   accounts.nbp — the persisted negative-balance-protection entitlement
--     flag consumed by Phase-19 Task 19.3.9 (spec §13.6c). RETAIL → true
--     (DEFAULT true matches the RETAIL default); the audited
--     categorization write path keeps nbp = (category='RETAIL') so the
--     column is a queryable fact, not a re-derivation.
--   appropriateness_assessments — the MiFID II appropriateness test
--     record gating leveraged/derivative instrument classes
--     (FORWARD|SWAP|NDF|OPTION; SPOT is exempt for every category).
--     outcome PASS|FAIL is derived server-side from score against the
--     pass mark; answers_json archives the questionnaire verbatim.
--     expires_at = assessed_at + 12 months, enforced at evaluation time
--     (a mid-session expiry blocks the NEXT order).

BEGIN;

CREATE TYPE client_category_enum AS ENUM
    ('RETAIL','PROFESSIONAL','ELIGIBLE_COUNTERPARTY');

ALTER TABLE accounts
    ADD COLUMN client_category client_category_enum NOT NULL DEFAULT 'RETAIL',
    ADD COLUMN nbp             BOOLEAN NOT NULL DEFAULT TRUE;

CREATE TABLE appropriateness_assessments (
    assessment_id    BIGSERIAL PRIMARY KEY,
    account_id       BIGINT       NOT NULL REFERENCES accounts (id),
    instrument_class VARCHAR(16)  NOT NULL                     -- gated classes only (SPOT exempt)
                     CHECK (instrument_class IN ('FORWARD','SWAP','NDF','OPTION')),
    outcome          VARCHAR(4)   NOT NULL CHECK (outcome IN ('PASS','FAIL')),
    score            SMALLINT     NOT NULL CHECK (score BETWEEN 0 AND 100),
    answers_json     JSONB        NOT NULL DEFAULT '{}'::jsonb, -- questionnaire verbatim
    assessed_at      TIMESTAMPTZ  NOT NULL,
    expires_at       TIMESTAMPTZ  NOT NULL,                     -- assessed_at + 12 months
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Latest-assessment-per-class resolution (the admission gate reads the
-- newest row for (account, class) and requires PASS + expires_at > now).
CREATE INDEX appropriateness_assessments_account_idx
    ON appropriateness_assessments (account_id, instrument_class, assessed_at DESC);

COMMIT;
