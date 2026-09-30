-- 033_create_sar_reports.up.sql
-- Phase-21 Tasks 21.3.3 (SAR generation) & 21.3.6 (FinCEN MSB + AML
-- program) — spec §14.1/§14.3; §24 #105/#108; §27.1 SAR-CTR & FinCEN
-- MSB matrix rows.
--
--   sar_reports            — the SAR/STR report lifecycle. DRAFT is
--                            machine- or officer-created; a Compliance
--                            Officer reviews (UNDER_REVIEW), a DISTINCT
--                            second officer approves (APPROVED —
--                            SAR_DUAL_CONTROL_REQUIRED on self-action),
--                            then filing records filed_at/filing_ref
--                            (FILED). filing_deadline = detected_at +
--                            30 days (FinCEN continuing-review clock).
--                            source_ref is the dedup anchor
--                            ('signal:{id}', 'hold:{id}', 'aml:{key}',
--                            'manual:{key}') — at-least-once NATS
--                            redelivery regenerates the same key and
--                            ON CONFLICT DO NOTHING makes drafting
--                            idempotent. Amendments are NEW rows linked
--                            by amends_id — FILED rows are immutable.
--   ctr_reports            — one row per (account, business day) whose
--                            aggregate cash movement reaches the $10,000
--                            FinCEN CTR threshold (§24 #105).
--   aml_monitoring_events  — deterministic monitoring-rule detections
--                            (structuring/smurfing, velocity, dormant
--                            reactivation, round-amount) carrying the
--                            suspicion score that feeds the SAR
--                            pipeline; dedup_key makes rule re-runs
--                            idempotent.
--   aml_program_artifacts  — FinCEN MSB program register: Form 107
--                            registration, risk assessments, versioned
--                            AML policies, training logs, officer
--                            designation, annual-review tracker
--                            (review_due_at).
--   aml_account_assessments— per-account CDD/EDD tier outcome the
--                            scoring engine maintains.
--
-- Retention (§19.12): regulatory records — no purge below the 7-year
-- compliance horizon; retention-policy registration rows land with the
-- Phase-09 enforcer catalogue.

BEGIN;

-- ---------------------------------------------------------------------------
-- sar_reports — §24 #108 dual-control filing workflow
-- ---------------------------------------------------------------------------

CREATE TABLE sar_reports (
    id              BIGSERIAL    PRIMARY KEY,
    trigger_type    VARCHAR(32)  NOT NULL CHECK (trigger_type IN (
        'MANUAL', 'SANCTIONS_MATCH', 'VELOCITY_ANOMALY', 'STRUCTURING',
        'SURVEILLANCE_SIGNAL', 'CTR', 'MONITORING_RULE', 'HOLD_ESCALATION')),
    account_id      BIGINT       REFERENCES accounts (id),
    subject_ref     VARCHAR(128),                       -- opaque ref when account unresolved (e.g. 'account_hash:12345')
    description     TEXT         NOT NULL,
    evidence        JSONB        NOT NULL DEFAULT '{}', -- immutable evidence bundle: signal/hold/event refs
    transaction_ids JSONB        NOT NULL DEFAULT '[]', -- funding_transactions ids included in the report
    status          VARCHAR(16)  NOT NULL DEFAULT 'DRAFT' CHECK (status IN
        ('DRAFT','UNDER_REVIEW','APPROVED','FILED','REJECTED')),
    source_ref      VARCHAR(160) NOT NULL,              -- deterministic dedup anchor
    detected_at     TIMESTAMPTZ  NOT NULL,              -- initial detection — drives the FinCEN clock
    filing_deadline TIMESTAMPTZ  NOT NULL,              -- detected_at + 30 days
    created_by      BIGINT,                             -- officer user id; NULL = machine draft
    reviewed_by     BIGINT,
    reviewed_at     TIMESTAMPTZ,
    review_note     TEXT,
    approved_by     BIGINT,
    approved_at     TIMESTAMPTZ,
    approval_note   TEXT,
    filed_by        BIGINT,
    filed_at        TIMESTAMPTZ,
    filing_ref      VARCHAR(128),                       -- BSA e-filing tracking number
    amends_id       BIGINT       REFERENCES sar_reports (id),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Idempotent drafting: one SAR per deterministic source event.
CREATE UNIQUE INDEX uq_sar_reports_source ON sar_reports (source_ref);

-- Officer queues + the deadline sweep: open reports oldest-deadline-first.
CREATE INDEX ix_sar_reports_status   ON sar_reports (status, filing_deadline);
CREATE INDEX ix_sar_reports_deadline ON sar_reports (filing_deadline)
    WHERE status IN ('DRAFT','UNDER_REVIEW','APPROVED');
CREATE INDEX ix_sar_reports_account  ON sar_reports (account_id, created_at DESC);

-- Filed SARs are immutable regulatory records — corrections file a NEW
-- sar_reports row via amends_id, never an UPDATE/DELETE of the original.
CREATE OR REPLACE FUNCTION sar_reports_filed_immutable() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status = 'FILED' THEN
        RAISE EXCEPTION 'sar_reports: FILED rows are immutable (file an amendment row with amends_id=%)', OLD.id
            USING ERRCODE = 'raise_exception';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER trg_sar_reports_immutable
    BEFORE UPDATE OR DELETE ON sar_reports
    FOR EACH ROW EXECUTE FUNCTION sar_reports_filed_immutable();

-- ---------------------------------------------------------------------------
-- ctr_reports — FinCEN Currency Transaction Report triggers
-- ---------------------------------------------------------------------------

CREATE TABLE ctr_reports (
    id             BIGSERIAL    PRIMARY KEY,
    account_id     BIGINT       NOT NULL REFERENCES accounts (id),
    business_date  DATE         NOT NULL,               -- UTC business day of the aggregate
    txn_count      INTEGER      NOT NULL,
    cash_in_usd    NUMERIC(28,8) NOT NULL DEFAULT 0,
    cash_out_usd   NUMERIC(28,8) NOT NULL DEFAULT 0,
    total_usd      NUMERIC(28,8) NOT NULL,
    unpriced_count INTEGER      NOT NULL DEFAULT 0,     -- non-USD legs without usd_amount — review-visible
    status         VARCHAR(16)  NOT NULL DEFAULT 'TRIGGERED'
                   CHECK (status IN ('TRIGGERED','REVIEWED','FILED','EXEMPT')),
    sar_id         BIGINT       REFERENCES sar_reports (id),
    detail         JSONB        NOT NULL DEFAULT '{}',  -- contributing funding_transaction ids
    reviewed_by    BIGINT,
    filed_at       TIMESTAMPTZ,
    filing_ref     VARCHAR(128),
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_ctr_reports_day ON ctr_reports (account_id, business_date);
CREATE INDEX ix_ctr_reports_status  ON ctr_reports (status, created_at DESC);
CREATE INDEX ix_ctr_reports_account ON ctr_reports (account_id, business_date DESC);

-- ---------------------------------------------------------------------------
-- aml_monitoring_events — rule detections feeding the SAR pipeline
-- ---------------------------------------------------------------------------

CREATE TABLE aml_monitoring_events (
    id            BIGSERIAL   PRIMARY KEY,
    rule_id       VARCHAR(32) NOT NULL CHECK (rule_id IN (
        'CTR_THRESHOLD','STRUCTURING','VELOCITY','DORMANT_REACTIVATION',
        'ROUND_AMOUNT','HIGH_RISK_JURISDICTION')),
    account_id    BIGINT      NOT NULL REFERENCES accounts (id),
    business_date DATE        NOT NULL,
    score         INTEGER     NOT NULL CHECK (score BETWEEN 0 AND 100),
    detail        JSONB       NOT NULL DEFAULT '{}',
    dedup_key     VARCHAR(160) NOT NULL,                  -- e.g. 'structuring:{account}:{date}'
    sar_id        BIGINT      REFERENCES sar_reports (id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_aml_events_dedup  ON aml_monitoring_events (dedup_key);
CREATE INDEX ix_aml_events_account       ON aml_monitoring_events (account_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- aml_program_artifacts — FinCEN MSB program register
-- ---------------------------------------------------------------------------

CREATE TABLE aml_program_artifacts (
    id            BIGSERIAL    PRIMARY KEY,
    artifact_type VARCHAR(32)  NOT NULL CHECK (artifact_type IN (
        'MSB_REGISTRATION',    -- FinCEN Form 107
        'RISK_ASSESSMENT',     -- enterprise AML risk assessment
        'POLICY',              -- versioned AML program policy
        'TRAINING',            -- annual AML training log
        'OFFICER_DESIGNATION', -- designated AML/BSA officer
        'ANNUAL_REVIEW')),     -- independent annual review tracker
    reference     VARCHAR(128),                          -- FinCEN filing/confirmation number
    title         VARCHAR(255) NOT NULL,
    version       VARCHAR(32),
    status        VARCHAR(16)  NOT NULL DEFAULT 'CURRENT'
                  CHECK (status IN ('CURRENT','SUPERSEDED','EXPIRED')),
    detail        JSONB        NOT NULL DEFAULT '{}',
    effective_at  TIMESTAMPTZ,
    review_due_at TIMESTAMPTZ,                           -- annual-review tracker due date
    recorded_by   BIGINT,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX ix_aml_artifacts_type   ON aml_program_artifacts (artifact_type, status);
CREATE INDEX ix_aml_artifacts_review ON aml_program_artifacts (review_due_at)
    WHERE status = 'CURRENT' AND review_due_at IS NOT NULL;

-- ---------------------------------------------------------------------------
-- aml_account_assessments — per-account CDD/EDD tier outcome
-- ---------------------------------------------------------------------------

CREATE TABLE aml_account_assessments (
    account_id   BIGINT       PRIMARY KEY REFERENCES accounts (id),
    cdd_level    VARCHAR(16)  NOT NULL DEFAULT 'CDD'
                 CHECK (cdd_level IN ('CDD','EDD','PROHIBITED')),
    score        INTEGER      NOT NULL DEFAULT 0 CHECK (score BETWEEN 0 AND 100),
    reasons      JSONB        NOT NULL DEFAULT '[]',
    assessed_by  BIGINT,                                -- NULL = rule engine
    assessed_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);

COMMIT;
