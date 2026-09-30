-- 054_regulatory_reporting.up.sql
-- Phase-21 Task 21.3.14 — EMIR REFIT & CFTC Parts 43/45 Reporting
-- Lifecycle (spec §5.32, §14.1a; §24 #169/#170). Shared file: §5.32 also
-- pins venue_members for Task 21.3.15 (the member/DEA register — the
-- table lands here so both owning tasks see one migration).
--
--   regulatory_report_events      — canonical reportable-event store.
--                                   One row per (uti, regime, report_seq):
--                                   NEWT lands seq 1, every lifecycle
--                                   event (MODI/VALU/MARU/TERM/CORR/ERRO
--                                   …) lands seq+1 with prior_uti links.
--                                   Immutable — corrections INSERT a
--                                   CORR row, never UPDATE the original.
--   regulatory_report_submissions — artifact journal: one row per
--                                   serialized, schema-validated report
--                                   artifact pushed toward a repository
--                                   (TR/SDR/ARM/APA). Every resubmission
--                                   is a NEW row (attempt+1) — the
--                                   immutable resubmission history the
--                                   §14.1a data-quality workflow keeps.
--   regulatory_report_acks        — immutable ACK/NACK ingest log
--                                   (repository synchronous ACKs +
--                                   asynchronous NACK/reconcile
--                                   feedback).
--   regulatory_report_breaks      — reconciliation-break tracking:
--                                   missing/duplicate/stale valuation/
--                                   identifier-collision/lifecycle-
--                                   divergence rows with a timed repair
--                                   queue (OPEN → REPAIRING → RESOLVED).
--   regulatory_schema_versions    — version-pinned official ruleset
--                                   registry: per (regulation, schema)
--                                   the version, effective window and
--                                   the required-field/format rules the
--                                   validator enforces before
--                                   submission.
--   venue_members                 — §5.32 member/DEA/sponsored-access
--                                   register (Task 21.3.15 consumes).
--   party_identifiers             — regulatory party identifiers per
--                                   account (LEI ISO 17442, national ID
--                                   per RTS 22 Annex II, decision-maker
--                                   id). The buyer/seller/decision-maker
--                                   decomposition source for RTS 22 and
--                                   counterparty-LEI source for EMIR/
--                                   CFTC. Missing identifiers quarantine
--                                   the report — never fabricate.
--   cftc_position_limits          — durable per-instrument CFTC position
--                                   limits + large-trader thresholds
--                                   consumed by Task 21.3.9 enforcement;
--                                   Task 21.3.27 owns the canonical
--                                   tabulated values.
--
-- Retention (spec §19.12 / MiFID II + CFTC record-keeping):
--   * All tables retained >= 5 years; corrections keep superseded rows.
--   The Phase-09 Task 9.3.22 retention enforcer MUST NOT purge below
--   the horizon; permitted deletion is archival-to-S3 first.

BEGIN;

-- ---------------------------------------------------------------------------
-- Enums-as-CHECKs (this codebase's dominant convention for small sets is
-- VARCHAR+CHECK — see trade_busts migration 051 — but the spec-level enum
-- types used elsewhere are PG ENUMs; these sets are regulatory and
-- effectively frozen, so ENUMs keep invalid values unrepresentable).
-- ---------------------------------------------------------------------------

CREATE TYPE reg_regime_enum AS ENUM (
    'EMIR_REFIT',   -- EU EMIR REFIT ISO 20022 TR reporting
    'CFTC_P43',     -- CFTC Part 43 real-time public dissemination
    'CFTC_P45',     -- CFTC Part 45 regulatory swap data reporting
    'MIFID2'        -- MiFID II RTS 22 transaction reporting
);

-- EMIR REFIT Action Types (ISO 20022 ActnTp) + CFTC lifecycle actions.
CREATE TYPE reg_action_enum AS ENUM (
    'NEWT',  -- new trade/position creation report
    'MODI',  -- modification
    'CORR',  -- correction
    'TERM',  -- termination (incl. early termination)
    'ERRO',  -- error report
    'REVI',  -- revive (re-open a wrongly terminated report)
    'VALU',  -- valuation update (mark-to-market/model)
    'MARU',  -- margin update
    'POSC',  -- position component update
    'ALOC',  -- allocation
    'CLRG',  -- clearing
    'PORT',  -- porting/compression transfer
    'LTR'    -- large-trader report (CFTC Part 17/20; venue extension)
);

-- Lifecycle event types shared by EMIR REFIT and CFTC 43/45.
CREATE TYPE reg_event_type_enum AS ENUM (
    'TRADE', -- execution
    'MOD',   -- post-execution modification
    'CORR',  -- correction event
    'VALU',  -- valuation event
    'MARG',  -- margin/collateral event
    'COMP',  -- compression
    'ALOC',  -- allocation
    'CLRG',  -- clearing
    'TERM',  -- termination/maturity
    'ERRO',  -- error/omission notification
    'NOAT',  -- non-actionable termination
    'RPT'    -- periodic/derived report (large trader, EOD collateral)
);

CREATE TYPE reg_event_status_enum AS ENUM (
    'RECORDED',    -- captured, validation pending
    'VALIDATED',   -- passed pinned schema/field rules — submittable
    'QUARANTINED', -- failed validation — held for repair, never sent
    'SUBMITTED',   -- artifact journal row dispatched to repository
    'ACCEPTED',    -- repository ACK received
    'REJECTED',    -- repository NACK received — enters repair queue
    'REPAIRED'     -- corrected + resubmitted after NACK/quarantine
);

CREATE TYPE reg_destination_enum AS ENUM (
    'TR',  -- EMIR trade repository (DTCC/REGIS-TR …)
    'SDR', -- CFTC swap data repository (CME/ICE/DTCC …)
    'ARM', -- MiFID II Approved Reporting Mechanism
    'APA'  -- MiFID II Approved Publication Arrangement
);

CREATE TYPE reg_submission_status_enum AS ENUM (
    'PENDING',     -- artifact generated, awaiting dispatch
    'SUBMITTED',   -- dispatched, awaiting repository verdict
    'ACKED',       -- accepted by repository
    'NACKED',      -- rejected by repository
    'QUARANTINED', -- validation failure — blocked before dispatch
    'SUPERSEDED'   -- replaced by a later attempt (immutable history)
);

CREATE TYPE reg_ack_status_enum AS ENUM (
    'ACK',   -- repository acceptance
    'NACK',  -- repository rejection
    'RECON'  -- reconciliation/feedback message (break candidates)
);

CREATE TYPE reg_break_type_enum AS ENUM (
    'MISSING',              -- internal open derivative lacks ACCEPTED repo row
    'DUPLICATE',            -- >1 live NEWT for one (uti, regime)
    'STALE_VALUATION',      -- open position moved, no VALU continuation
    'STALE_MARGIN',         -- margin moved, no MARU continuation
    'ID_COLLISION',         -- UTI/USI bound to two distinct trade ids
    'LIFECYCLE_DIVERGENCE', -- TERMed repo side vs open internal (or inverse)
    'NACK_REPAIR',          -- repository rejection awaiting repair
    'VALIDATION'            -- quarantined before dispatch
);

CREATE TYPE reg_break_status_enum AS ENUM (
    'OPEN', 'REPAIRING', 'RESOLVED', 'WONT_FIX'
);

-- ---------------------------------------------------------------------------
-- Canonical event store
-- ---------------------------------------------------------------------------

CREATE TABLE regulatory_report_events (
    event_id           BIGSERIAL PRIMARY KEY,
    uti                VARCHAR(52)  NOT NULL,             -- ISO 23897 UTI
    usi                VARCHAR(64),                       -- CFTC USI (namespace+id)
    upi                VARCHAR(12),                       -- DSB UPI (ISO 4914)
    prior_uti          VARCHAR(52),                       -- prior-ID link (compression/
    prior_usi          VARCHAR(64),                       --   porting chains)
    regime             reg_regime_enum      NOT NULL,
    action_type        reg_action_enum      NOT NULL,
    event_type         reg_event_type_enum  NOT NULL,
    report_seq         INTEGER              NOT NULL DEFAULT 1 CHECK (report_seq >= 1),
    trade_id           BIGINT,                            -- logical ref → trades.id
                                                        --   (partitioned table — no FK,
                                                        --   same discipline as 051)
    position_id        BIGINT,                            -- logical ref → positions.id
    instrument_id      BIGINT,
    instrument_code    VARCHAR(64)  NOT NULL,             -- venue instrument code
                                                        --   (NOT ISIN — spot/FX
                                                        --   derivatives carry ISO
                                                        --   pair/product codes)
    instrument_type    VARCHAR(8),                        -- SPOT|FORWARD|SWAP|NDF|OPTION
    account_id         BIGINT,                            -- venue-side reporting account
    counterparty_account_id BIGINT,                       -- opposite side account
    buyer_lei          VARCHAR(20),                       -- ISO 17442, when known
    seller_lei         VARCHAR(20),
    buyer_id_type      VARCHAR(12),                       -- 'LEI'|'NATIONAL_ID'|'INTC'
    buyer_id           VARCHAR(64),                       -- RTS 22 buyer identifier
    seller_id_type     VARCHAR(12),
    seller_id          VARCHAR(64),
    decision_maker_type VARCHAR(12),                      -- 'LEI'|'NATIONAL_ID'|'ALGO'
    decision_maker_id  VARCHAR(64),                       -- RTS 22 investment decision
    trader_id          VARCHAR(64),                       -- execution decision-maker
    algo_id            VARCHAR(64),                       -- RTS 22 algo identifier
    venue_mic          VARCHAR(4),                        -- segment MIC of this venue
    jurisdiction       VARCHAR(16),                       -- EU|US|… reporter scope
    dual_sided         BOOLEAN      NOT NULL DEFAULT false, -- EMIR dual-sided flag
    price              DECIMAL(28,8),
    quantity           DECIMAL(28,8),
    notional           DECIMAL(28,8),
    currency           VARCHAR(3),
    valuation          JSONB        NOT NULL DEFAULT '{}'::jsonb, -- MtM, ccy
    margin             JSONB        NOT NULL DEFAULT '{}'::jsonb, -- IM/VM/collateral
    clearing           JSONB        NOT NULL DEFAULT '{}'::jsonb,
    confirmation       JSONB        NOT NULL DEFAULT '{}'::jsonb,
    allocation         JSONB        NOT NULL DEFAULT '{}'::jsonb,
    payload            JSONB        NOT NULL DEFAULT '{}'::jsonb, -- regime record
    schema_version     VARCHAR(32),                       -- registry-pinned version
    status             reg_event_status_enum NOT NULL DEFAULT 'RECORDED',
    validation_errors  JSONB        NOT NULL DEFAULT '[]'::jsonb, -- pinned-rule misses
    dissemination_due_at TIMESTAMPTZ,                     -- CFTC P43 15-min SLA /
                                                        --   APA 1-min deadline
    supersedes_event_id BIGINT REFERENCES regulatory_report_events (event_id),
    event_ts           TIMESTAMPTZ  NOT NULL,             -- business-event time
    reported_at        TIMESTAMPTZ,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (uti, regime, report_seq)
);
CREATE INDEX reg_events_uti_ix      ON regulatory_report_events (uti, regime, report_seq);
CREATE INDEX reg_events_trade_ix    ON regulatory_report_events (trade_id) WHERE trade_id IS NOT NULL;
CREATE INDEX reg_events_status_ix   ON regulatory_report_events (status, regime);
CREATE INDEX reg_events_account_ix  ON regulatory_report_events (account_id, event_ts DESC);
CREATE INDEX reg_events_created_ix  ON regulatory_report_events (created_at, event_id);
CREATE INDEX reg_events_dissem_ix   ON regulatory_report_events (dissemination_due_at)
    WHERE status IN ('VALIDATED','SUBMITTED');
COMMENT ON TABLE regulatory_report_events IS
    'Task 21.3.14 canonical reportable-event store (spec §5.32/§14.1a): '
    'UTI/USI/DSB UPI + prior-ID links, action/event lifecycle, party '
    'identifiers, valuation/margin/clearing/confirmation/allocation and '
    'validation outcome per (uti, regime, report_seq). IMMUTABLE — '
    'corrections append CORR rows (supersedes_event_id links the chain), '
    'never UPDATE the original. RETENTION: >= 5 years.';

-- ---------------------------------------------------------------------------
-- Artifact journal — serialized + validated report instances per event
-- ---------------------------------------------------------------------------

CREATE TABLE regulatory_report_submissions (
    report_submission_id BIGSERIAL PRIMARY KEY,
    event_id         BIGINT              NOT NULL
                     REFERENCES regulatory_report_events (event_id),
    regime           reg_regime_enum     NOT NULL,
    destination      reg_destination_enum NOT NULL,
    attempt          INTEGER             NOT NULL DEFAULT 1 CHECK (attempt >= 1),
    schema_name      VARCHAR(64)         NOT NULL,    -- auth.030.001.05 / CFTC-P45 …
    schema_version   VARCHAR(32)         NOT NULL,
    payload          JSONB               NOT NULL,    -- serialized report artifact
    payload_xml      TEXT,                            -- ISO 20022 XML when produced
    payload_hash     CHAR(64)            NOT NULL,    -- sha256(payload_xml || payload)
    status           reg_submission_status_enum NOT NULL DEFAULT 'PENDING',
    external_ref     VARCHAR(128),                    -- repository receipt / report id
    error_code       VARCHAR(64),
    error_text       VARCHAR(1024),
    batch_id         VARCHAR(64),                     -- ARM T+1 batch grouping
    submitted_at     TIMESTAMPTZ,
    resolved_at      TIMESTAMPTZ,
    created_at       TIMESTAMPTZ         NOT NULL DEFAULT now(),
    UNIQUE (event_id, destination, attempt)
);
CREATE INDEX reg_report_submissions_status_ix
    ON regulatory_report_submissions (status, destination);
CREATE INDEX reg_report_submissions_event_ix
    ON regulatory_report_submissions (event_id, regime);
COMMENT ON TABLE regulatory_report_submissions IS
    'Task 21.3.14 artifact journal (spec §5.32): one immutable row per '
    'serialized, schema-pinned report artifact per dispatch attempt — the '
    'ACK/NACK/correction resubmission history. Corrections land as a new '
    'event row; re-dispatch lands attempt+1 here. Transport wire attempts '
    'are ledgered in regulatory_submissions (migration 059).';

-- ---------------------------------------------------------------------------
-- ACK/NACK ingest log (immutable)
-- ---------------------------------------------------------------------------

CREATE TABLE regulatory_report_acks (
    ack_id             BIGSERIAL PRIMARY KEY,
    report_submission_id BIGINT            NOT NULL
                     REFERENCES regulatory_report_submissions (report_submission_id),
    event_id           BIGINT              NOT NULL
                     REFERENCES regulatory_report_events (event_id),
    ack_status         reg_ack_status_enum NOT NULL,
    ack_code           VARCHAR(64),                    -- repository error/ack code
    ack_text           VARCHAR(1024),
    external_ref       VARCHAR(128),
    payload            JSONB               NOT NULL DEFAULT '{}'::jsonb,
    received_at        TIMESTAMPTZ         NOT NULL DEFAULT now()
);
CREATE INDEX reg_acks_submission_ix
    ON regulatory_report_acks (report_submission_id, received_at DESC);
CREATE INDEX reg_acks_event_ix ON regulatory_report_acks (event_id);
COMMENT ON TABLE regulatory_report_acks IS
    'Immutable repository acknowledgement log (spec §14.1a): synchronous '
    'ACKs, asynchronous NACKs and reconciliation/feedback messages. Rows '
    'are never updated — a later message appends a new row.';

-- ---------------------------------------------------------------------------
-- Reconciliation-break tracking + timed repair queue
-- ---------------------------------------------------------------------------

CREATE TABLE regulatory_report_breaks (
    break_id     BIGSERIAL PRIMARY KEY,
    event_id     BIGINT              REFERENCES regulatory_report_events (event_id),
    uti          VARCHAR(52),
    regime       reg_regime_enum,
    break_type   reg_break_type_enum  NOT NULL,
    status       reg_break_status_enum NOT NULL DEFAULT 'OPEN',
    detected_by  VARCHAR(16)          NOT NULL,       -- 'reconciler'|'ack'|'validator'
    detail       JSONB                NOT NULL DEFAULT '{}'::jsonb,
    sla_due_at   TIMESTAMPTZ          NOT NULL,       -- repair window (T+1 close / 2h)
    detected_at  TIMESTAMPTZ          NOT NULL DEFAULT now(),
    resolved_at  TIMESTAMPTZ,
    resolved_by  BIGINT,                              -- admin user id
    notes        VARCHAR(1024)
);
CREATE INDEX reg_breaks_open_ix
    ON regulatory_report_breaks (status, break_type) WHERE status <> 'RESOLVED';
CREATE INDEX reg_breaks_event_ix ON regulatory_report_breaks (event_id);
COMMENT ON TABLE regulatory_report_breaks IS
    'Task 21.3.14 reconciliation-break ledger (spec §14.1a, §24 #169-170): '
    'missing/duplicate/stale-valuation/identifier-collision/lifecycle-'
    'divergence and NACK-repair items with a timed repair queue. OPEN '
    'rows past sla_due_at escalate to the Compliance Officer.';

-- ---------------------------------------------------------------------------
-- Version-pinned schema/ruleset registry
-- ---------------------------------------------------------------------------

CREATE TABLE regulatory_schema_versions (
    schema_id      BIGSERIAL PRIMARY KEY,
    regulation     VARCHAR(24)  NOT NULL,   -- EMIR_REFIT|CFTC_P43|CFTC_P45|MIFID2_RTS22|MIFID2_RTS1
    schema_name    VARCHAR(64)  NOT NULL,   -- auth.030.001.05 / auth.016.001.05 / CFTC-P45-JSON …
    version        VARCHAR(32)  NOT NULL,   -- official ruleset version stamp
    rules_ref      VARCHAR(96)  NOT NULL,   -- pinned rulebook ref (e.g. 'ESMA-EMIR-REFIT-2024-12',
                                            -- 'CFTC-Part45-2022-11', 'ESMA-RTS22-2017-07')
    required_fields JSONB       NOT NULL,   -- pinned required field list the validator enforces
    effective_from DATE         NOT NULL,
    effective_to   DATE,                    -- NULL = currently effective
    active         BOOLEAN      NOT NULL DEFAULT true,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (regulation, schema_name, version)
);
COMMENT ON TABLE regulatory_schema_versions IS
    'Version-pinned official reporting rulesets (spec §5.32): which '
    'regulation × schema × version a submission was generated and '
    'validated against, plus the effective window — a repository schema '
    'change deactivates the old row and inserts the new pinned version '
    '(edge case: repository schema change, Phase-21 Task 21.3.14 SDD).';

-- Seed the pinned rulesets the validators consult (required_fields are
-- the mandatory-field names checked before dispatch; the full official
-- schema validation runs repository-side, mirrored in our pre-flight).
INSERT INTO regulatory_schema_versions
    (regulation, schema_name, version, rules_ref, required_fields, effective_from) VALUES
    ('EMIR_REFIT', 'auth.030.001.05', '2024-EMIR-REFIT', 'ESMA-EMIR-REFIT-ITS-2024',
     '["uti","upi","counterparty1_lei","counterparty2_id","action_type","event_type","event_ts","instrument_code","price","quantity","notional","currency","venue_mic","valuation","margin"]'::jsonb,
     '2024-01-01'),
    ('CFTC_P43', 'CFTC-P43-DISSEMINATION', '2022-PART43', 'CFTC-Part43-2022-11',
     '["usi","uti","asset_class","instrument_code","price","quantity","currency","event_ts","venue_mic"]'::jsonb,
     '2022-11-01'),
    ('CFTC_P45', 'CFTC-P45-SDR', '2022-PART45', 'CFTC-Part45-2022-11',
     '["uti","usi","upi","prior_uti","action_type","event_type","event_ts","instrument_code","price","quantity","currency","counterparty1_lei","counterparty2_id"]'::jsonb,
     '2022-11-01'),
    ('MIFID2_RTS22', 'auth.016.001.05', 'RTS22-2017', 'ESMA-RTS22-2017-07',
     '["tvtc","executing_entity_lei","buyer_id","seller_id","trading_datetime","venue_mic","instrument_code","price","quantity","currency","decision_maker_id","trader_id"]'::jsonb,
     '2018-01-03'),
    ('MIFID2_RTS1', 'RTS1-POST-TRADE', 'RTS12-2017', 'ESMA-RTS1-2-2017-07',
     '["trade_id","instrument_code","price","quantity","currency","trading_datetime","venue_mic","publication_datetime"]'::jsonb,
     '2018-01-03');

-- ---------------------------------------------------------------------------
-- Party identifier registry (buyer/seller/decision-maker decomposition)
-- ---------------------------------------------------------------------------

CREATE TABLE party_identifiers (
    party_id           BIGSERIAL PRIMARY KEY,
    account_id         BIGINT       NOT NULL REFERENCES accounts (id),
    lei                VARCHAR(20),                       -- ISO 17442
    national_id_type   VARCHAR(16),                       -- RTS 22 Annex II
                                                        --   ('PASSPORT','NATIONAL_ID','TAX')
    national_id        VARCHAR(64),
    decision_maker_id  VARCHAR(64),                       -- per-account default
    decision_maker_type VARCHAR(12)                       -- 'LEI'|'NATIONAL_ID'
                       CHECK (decision_maker_type IN ('LEI','NATIONAL_ID')),
    updated_by         BIGINT,                            -- admin user id (compliance)
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (account_id)
);
COMMENT ON TABLE party_identifiers IS
    'Regulatory party identifiers per account (spec §5.32, Task 21.3.14): '
    'LEI (ISO 17442 checksum-validated at write), RTS 22 natural-person '
    'national ID and the account-level decision-maker id. Missing party '
    'data quarantines the report — never fabricated.';

-- ---------------------------------------------------------------------------
-- CFTC position limits + large-trader thresholds (Task 21.3.9)
-- ---------------------------------------------------------------------------

CREATE TABLE cftc_position_limits (
    limit_id               BIGSERIAL PRIMARY KEY,
    instrument_id          BIGINT       REFERENCES instruments (id), -- NULL → class default
    instrument_type        VARCHAR(8)   NOT NULL DEFAULT 'SWAP',       -- FORWARD|SWAP|NDF|OPTION
    currency_pair          VARCHAR(8),                                 -- 'EUR/USD' class key
    spot_month_limit       DECIMAL(28,8),                              -- NULL = no spot-month cap
    all_months_limit       DECIMAL(28,8),                              -- aggregate notional cap
    large_trader_threshold DECIMAL(28,8) NOT NULL,                     -- reportable level
    currency               VARCHAR(3)   NOT NULL DEFAULT 'USD',
    effective_from         DATE         NOT NULL,
    effective_to           DATE,
    source                 VARCHAR(96)  NOT NULL DEFAULT 'CFTC-Part150',
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (instrument_id, effective_from)
);
COMMENT ON TABLE cftc_position_limits IS
    'Task 21.3.9 CFTC position-limit + large-trader-threshold store '
    '(spec §14.1a; the §14.11 tabulated values are curated by Task '
    '21.3.27). instrument_id NULL rows are instrument-type class '
    'defaults. Limits are notional-denominated; enforcement sums open '
    'per-account per-instrument exposure against the active row.';

-- ---------------------------------------------------------------------------
-- venue_members — spec §5.32 member/DEA register (consumed by Task 21.3.15)
-- ---------------------------------------------------------------------------

CREATE TABLE venue_members (
    member_id            BIGSERIAL PRIMARY KEY,
    legal_name           VARCHAR(256) NOT NULL,
    lei                  VARCHAR(20)  NOT NULL,
    regulatory_status    VARCHAR(32)  NOT NULL DEFAULT 'PENDING',
    access_model         VARCHAR(12)  NOT NULL
                         CHECK (access_model IN ('MEMBER','DEA','SPONSORED')),
    approved_products    JSONB        NOT NULL DEFAULT '[]'::jsonb,
    approved_ports       JSONB        NOT NULL DEFAULT '[]'::jsonb,
    due_diligence_status VARCHAR(24)  NOT NULL DEFAULT 'PENDING',
    due_diligence_evidence JSONB      NOT NULL DEFAULT '{}'::jsonb,
    admission_decision   VARCHAR(16)  NOT NULL DEFAULT 'PENDING'
                         CHECK (admission_decision IN ('PENDING','APPROVED','DENIED')),
    annual_review_due    DATE,
    suspended            BOOLEAN      NOT NULL DEFAULT false,
    suspended_at         TIMESTAMPTZ,
    suspension_reason    VARCHAR(512),
    agreements           JSONB        NOT NULL DEFAULT '[]'::jsonb,
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (lei)
);
COMMENT ON TABLE venue_members IS
    'Spec §5.32 venue member/DEA/sponsored-access register — Task '
    '21.3.15 owns the admission/review/suspension workflow; the table '
    'ships with migration 054 because §5.32 assigns it there.';

COMMIT;
