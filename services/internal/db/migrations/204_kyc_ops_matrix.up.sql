-- 204_kyc_ops_matrix.up.sql
-- Phase-12 Task 12.3.13 — KYC operations matrix (spec §12.7, §24 #341).
--
-- Two tables:
--   kyc_tier_policies — per-tier scalars: liveness/biometric flags,
--     PEP/adverse-media rescreen cadence (DAILY T2/institutional,
--     WEEKLY T1 — supersedes "on schedule" vagueness), re-verification
--     period, 24h manual-review SLA, applicant risk-score step-up /
--     decline thresholds, §14.2 USD limits.
--   kyc_ops_matrix — vendor × document_type × jurisdiction requirement
--     rows. doc_group groups alternatives: a submission must supply at
--     least one document per REQUIRED group for the tier. jurisdiction
--     '*' is the default; an exact ISO alpha-2 row overrides/extends.
--
-- vendor 'MANUAL' is the honest value until a document-verification
-- vendor integration lands (seam: Phase-14 routes rows to a provider).
-- Liveness/biometric flags are requirements data — the actual liveness
-- check is provider-side; the venue records the requirement, not a fake
-- result.

BEGIN;

CREATE TABLE kyc_tier_policies (
    tier                    VARCHAR(16) PRIMARY KEY
                            CHECK (tier IN ('T0','T1','T2','INSTITUTIONAL')),
    description             TEXT        NOT NULL,
    liveness_required       BOOLEAN     NOT NULL,
    biometric_required      BOOLEAN     NOT NULL,
    rescreen_cadence        VARCHAR(8)  NOT NULL  -- PEP / adverse-media rescreening
                            CHECK (rescreen_cadence IN ('NONE','WEEKLY','DAILY')),
    reverify_months         INTEGER,              -- NULL = no periodic re-verification
    manual_review_sla_hours INTEGER     NOT NULL DEFAULT 24,
    step_up_score           SMALLINT    NOT NULL, -- intake risk_score >= → step-up checks (liveness/EDD)
    decline_score           SMALLINT    NOT NULL, -- intake risk_score >= → decline path (manual adjudication)
    daily_withdrawal_usd    NUMERIC(28,8),        -- §14.2 cap; 0 = disabled, NULL = negotiated
    daily_trading_usd       NUMERIC(28,8),        -- 0 = no trading, NULL = unlimited
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE kyc_ops_matrix (
    id                   BIGSERIAL PRIMARY KEY,
    tier                 VARCHAR(16) NOT NULL REFERENCES kyc_tier_policies (tier),
    jurisdiction         VARCHAR(2)  NOT NULL,    -- ISO alpha-2; '*' = default
    vendor               VARCHAR(32) NOT NULL,    -- verification vendor; 'MANUAL' = ops review
    document_type        VARCHAR(40) NOT NULL,
    doc_group            VARCHAR(16) NOT NULL     -- IDENTITY|ADDRESS|CORPORATE|FUNDS|LIVENESS|TAX
                         CHECK (doc_group IN ('IDENTITY','ADDRESS','CORPORATE','FUNDS','LIVENESS','TAX')),
    required             BOOLEAN     NOT NULL DEFAULT TRUE, -- the GROUP is required (one member doc satisfies)
    max_doc_age_days     INTEGER,                           -- NULL = no freshness rule
    doc_expiry_lead_days INTEGER,                           -- re-verification trigger N days pre doc-expiry
    notes                TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tier, jurisdiction, document_type)
);

INSERT INTO kyc_tier_policies
    (tier, description, liveness_required, biometric_required, rescreen_cadence,
     reverify_months, manual_review_sla_hours, step_up_score, decline_score,
     daily_withdrawal_usd, daily_trading_usd) VALUES
('T0', 'Email-only — no verification; no trading, no withdrawals (§14.2)',
 false, false, 'NONE', NULL, 24, 40, 80, 0, 0),
('T1', 'ID + address verification — $10K/day withdrawal, $10K/day trading',
 true, false, 'WEEKLY', NULL, 24, 40, 80, 10000, 10000),
('T2', 'Full KYC + source of funds — $100K/day withdrawal, unlimited trading',
 true, true, 'DAILY', 12, 24, 40, 80, 100000, NULL),
('INSTITUTIONAL', 'Corporate KYB + manual review — negotiated limits',
 true, true, 'DAILY', 24, 24, 40, 80, NULL, NULL);

-- Default ('*') requirement matrix. One document per REQUIRED doc_group.
INSERT INTO kyc_ops_matrix
    (tier, jurisdiction, vendor, document_type, doc_group, required,
     max_doc_age_days, doc_expiry_lead_days, notes) VALUES
-- T1: identity + address proof + liveness (selfie match).
('T1','*','MANUAL','PASSPORT',        'IDENTITY', true, NULL, 30, 'one identity document required'),
('T1','*','MANUAL','NATIONAL_ID',     'IDENTITY', true, NULL, 30, NULL),
('T1','*','MANUAL','DRIVING_LICENCE', 'IDENTITY', true, NULL, 30, NULL),
('T1','*','MANUAL','UTILITY_BILL',    'ADDRESS',  true, 90, NULL, 'address proof ≤ 90 days old'),
('T1','*','MANUAL','BANK_STATEMENT',  'ADDRESS',  true, 90, NULL, NULL),
('T1','*','MANUAL','LIVENESS_SELFIE', 'LIVENESS', true, NULL, NULL, 'selfie match — provider-side check'),
-- T2: T1 set + source of funds.
('T2','*','MANUAL','PASSPORT',        'IDENTITY', true, NULL, 30, NULL),
('T2','*','MANUAL','NATIONAL_ID',     'IDENTITY', true, NULL, 30, NULL),
('T2','*','MANUAL','DRIVING_LICENCE', 'IDENTITY', true, NULL, 30, NULL),
('T2','*','MANUAL','UTILITY_BILL',    'ADDRESS',  true, 90, NULL, NULL),
('T2','*','MANUAL','BANK_STATEMENT',  'ADDRESS',  true, 90, NULL, NULL),
('T2','*','MANUAL','LIVENESS_SELFIE', 'LIVENESS', true, NULL, NULL, 'biometric bind to identity doc'),
('T2','*','MANUAL','SOURCE_OF_FUNDS', 'FUNDS',    true, 180, NULL, 'declaration + evidence (§14.2)'),
-- INSTITUTIONAL: corporate KYB — all manual review.
('INSTITUTIONAL','*','MANUAL','CERTIFICATE_OF_INCORPORATION','CORPORATE', true, NULL, NULL, 'KYB — corporate docs'),
('INSTITUTIONAL','*','MANUAL','CORPORATE_REGISTRY_EXTRACT',  'CORPORATE', true, 90,  NULL, 'registry extract ≤ 90 days'),
('INSTITUTIONAL','*','MANUAL','UBO_DECLARATION',             'CORPORATE', true, NULL, NULL, 'UBO ≥25% disclosure'),
('INSTITUTIONAL','*','MANUAL','SIGNATORY_ID',                'IDENTITY',  true, NULL, 30, 'authorised signatory passport/ID'),
('INSTITUTIONAL','*','MANUAL','SOURCE_OF_FUNDS',             'FUNDS',     true, 180, NULL, NULL),
-- Jurisdiction overlay: US-resident applicants must also file a tax
-- self-certification (W-9 US persons; W-8BEN/W-8BEN-E is the non-US
-- equivalent collected at T1+ regardless — see tax_self_certifications,
-- migration 205).
('T1','US','MANUAL','TAX_SELF_CERT_W9','TAX', true, NULL, NULL, 'US persons file W-9 (TIN validated)'),
('T2','US','MANUAL','TAX_SELF_CERT_W9','TAX', true, NULL, NULL, NULL),
('INSTITUTIONAL','US','MANUAL','TAX_SELF_CERT_W9','TAX', true, NULL, NULL, 'entity → W-9');

COMMIT;
