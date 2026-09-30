-- 243_gdpr_geo.up.sql
-- Phase-21 Task 21.3.7 — GDPR data-subject rights + geo-block policy
-- (spec §14.13 consent/erasure, §23 GEO_BLOCKED; Phase-21 AC geo-block
-- restricted jurisdictions + IP-based detection).
--
--   account_consent_states  — current revocable-consent state, one row
--                             per (account, purpose, channel). This is
--                             the Task 21.3.7 prescribed shape
--                             (account_id, purpose, channel, state,
--                             updated_at) that internal/analytics
--                             marketing cohorts read. Deliberately
--                             separate from the 239 account_consents
--                             document-acknowledgement ledger (Task
--                             21.3.28): document acks are append-only
--                             per-doc_ref evidence, GDPR consent is a
--                             revocable per-purpose switch.
--
--   account_consent_events  — append-only transition ledger (evidence
--                             of grant AND withdrawal, both directions).
--
--   gdpr_requests           — export/erasure request lifecycle. Partial
--                             unique index dedups concurrent re-requests:
--                             one open (PENDING|PROCESSING) request per
--                             (account, kind).
--
--   geo_jurisdiction_policies — per-country action table consumed by
--                             the gateway geo gate. BLOCK = no API
--                             surface at all; RETAIL_BLOCK = mutating
--                             requests refused for retail clients
--                             (spec §27 R15: US retail prohibited, US
--                             institutional flow served + reported);
--                             ALLOW = recorded-but-open jurisdiction.
--                             Fail-closed: a country with no row AND
--                             no resolver signal is treated as ALLOW
--                             only for non-restricted traffic — the
--                             service layer, not this table, owns the
--                             resolver-miss decision.
--
-- Retention: gdpr_requests + account_consent_events are legal-hold
-- evidence (§19.12 7-year horizon); erasure pseudonymises the USER
-- row, never this ledger.

BEGIN;

CREATE TABLE account_consent_states (
    account_id BIGINT      NOT NULL REFERENCES accounts (id),
    purpose    VARCHAR(32) NOT NULL CHECK (purpose IN (
        'MARKETING',      -- promotions, newsletters, outbound marketing
        'ANALYTICS',      -- usage analytics / non-essential telemetry
        'DATA_SHARING')), -- third-party data sharing
    channel    VARCHAR(32) NOT NULL DEFAULT 'ALL' CHECK (channel IN (
        'ALL', 'EMAIL', 'PUSH', 'SMS', 'IN_APP')),
    state      VARCHAR(16) NOT NULL CHECK (state IN ('GRANTED', 'WITHDRAWN')),
    updated_by BIGINT      NOT NULL,               -- user id performing the change
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata   JSONB       NOT NULL DEFAULT '{}',  -- surface/locale evidence
    PRIMARY KEY (account_id, purpose, channel)
);

CREATE TABLE account_consent_events (
    id         BIGSERIAL   PRIMARY KEY,
    account_id BIGINT      NOT NULL REFERENCES accounts (id),
    purpose    VARCHAR(32) NOT NULL,
    channel    VARCHAR(32) NOT NULL,
    state      VARCHAR(16) NOT NULL CHECK (state IN ('GRANTED', 'WITHDRAWN')),
    actor      BIGINT      NOT NULL,
    source     VARCHAR(16) NOT NULL DEFAULT 'API',  -- API | ADMIN | ERASURE
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ix_consent_events_account
    ON account_consent_events (account_id, created_at DESC, id DESC);

CREATE TABLE gdpr_requests (
    id             BIGSERIAL   PRIMARY KEY,
    account_id     BIGINT      NOT NULL REFERENCES accounts (id),
    user_id        BIGINT      NOT NULL,
    kind           VARCHAR(16) NOT NULL CHECK (kind IN ('EXPORT', 'ERASE')),
    status         VARCHAR(16) NOT NULL DEFAULT 'PENDING' CHECK (status IN (
        'PENDING', 'PROCESSING', 'COMPLETED', 'FAILED', 'REJECTED')),
    detail         JSONB       NOT NULL DEFAULT '{}', -- export manifest / erasure carve-out list
    artifact_ref   VARCHAR(512),                      -- object-store ref for large exports
    sha256         CHAR(64),                          -- export artifact digest
    requested_by   BIGINT      NOT NULL,
    reviewed_by    BIGINT,                            -- officer who processed erasure
    failure_reason TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at   TIMESTAMPTZ
);
-- Idempotent re-request: at most one open request per (account, kind).
CREATE UNIQUE INDEX ux_gdpr_requests_open
    ON gdpr_requests (account_id, kind)
    WHERE status IN ('PENDING', 'PROCESSING');
CREATE INDEX ix_gdpr_requests_account
    ON gdpr_requests (account_id, created_at DESC);

CREATE TABLE geo_jurisdiction_policies (
    country_code CHAR(2)     PRIMARY KEY,  -- ISO 3166-1 alpha-2
    action       VARCHAR(16) NOT NULL CHECK (action IN ('BLOCK', 'RETAIL_BLOCK', 'ALLOW')),
    reason       TEXT        NOT NULL,
    updated_by   BIGINT,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Baseline restricted list (Task 21.3.7 seeds): OFAC comprehensively
-- sanctioned / FATF call-for-action jurisdictions are hard-blocked; the
-- US row encodes §27 R15 (retail blocked, institutional served).
INSERT INTO geo_jurisdiction_policies (country_code, action, reason) VALUES
    ('IR', 'BLOCK', 'OFAC comprehensive sanctions — Iran'),
    ('KP', 'BLOCK', 'OFAC comprehensive sanctions + FATF call-for-action — North Korea'),
    ('CU', 'BLOCK', 'OFAC comprehensive sanctions — Cuba'),
    ('SY', 'BLOCK', 'OFAC comprehensive sanctions — Syria'),
    ('MM', 'BLOCK', 'FATF call-for-action — Myanmar'),
    ('US', 'RETAIL_BLOCK', 'spec §27 R15 — US retail prohibited; institutional flow served and reported'),
    ('GB', 'ALLOW', 'home-adjacent venue jurisdiction — recorded, open'),
    ('DE', 'ALLOW', 'EEA venue jurisdiction — recorded, open')
ON CONFLICT (country_code) DO NOTHING;

COMMIT;
