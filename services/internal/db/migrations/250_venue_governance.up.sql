-- 250_venue_governance.up.sql
-- Phase-21 Task 21.3.15 — Regulated-Venue Membership, Rule Enforcement
-- & CCO Controls (spec §5.32/§14.1b; §24 #174; §27.1 Regulated Venue
-- License Governance matrix row → JURISDICTION_UNLICENSED /
-- ANNUAL_ATTESTATION_OVERDUE).
--
-- The member/DEA/sponsored register itself is venue_members (migration
-- 054 — §5.32 pins it there); this file adds the termination and
-- jurisdiction columns plus every satellite the governance workflows
-- need:
--
--   venue_member_events        — immutable member lifecycle ledger:
--                                registration, due-diligence updates,
--                                agreements, product/port approval,
--                                admission decision, suspension,
--                                reinstatement, termination, appeal and
--                                appeal decision. Rows are never
--                                updated — the append-only evidence
--                                chain the AC "suspension/termination
--                                and appeals" audit trail requires.
--   venue_member_reviews       — annual/ad-hoc member risk reviews
--                                (outcome + findings + next-review date).
--   venue_rulebooks            — versioned rulebook + product terms:
--                                DRAFT → FILED → APPROVED → EFFECTIVE →
--                                SUPERSEDED/WITHDRAWN with regulator
--                                filing status; EFFECTIVE is refused
--                                before required approvals (service-
--                                enforced, CHECK documents intent).
--   venue_rule_notices         — participant notices per rulebook
--                                version (member_id NULL = broadcast).
--   venue_rule_acks            — member acknowledgement evidence per
--                                rulebook/notice.
--   venue_interventions        — real-time market-control + emergency
--                                record: limits, halts, cancellations/
--                                corrections, information requests,
--                                position accountability, emergency
--                                rules.
--   venue_cases                — investigation/disciplinary case
--                                workflow.
--   venue_case_evidence        — immutable evidence attachments
--                                (sha256-pinned refs).
--   venue_conflicts            — conflicts-of-interest register with
--                                recusal tracking.
--   venue_self_assessments     — annual RTS 7 / SEF system-safeguard
--                                self-assessment: assembled control
--                                evidence, exceptions, financial-resource
--                                attestation, unresolved remediation.
--   cco_reports                — CCO annual report (the board pack's
--                                `cco_report` section source probes this
--                                table: count + max(id)).
--   venue_launch_prerequisites — licensing / regulator authorization /
--                                legal opinions / board+CCO appointments /
--                                minimum financial resources — the
--                                launch-gate checklist (AC: "production
--                                launch gate blocks when required venue
--                                authorization or governance attestations
--                                are absent"). scope='GLOBAL' rows gate
--                                the venue; jurisdictional LICENSE /
--                                REGULATOR_AUTH rows gate member trading
--                                for that jurisdiction.
--
-- Retention (spec §19.12 / MiFID II RTS 7 + CFTC SEF record-keeping):
--   all rows retained >= 5 years; lifecycle history is append-only —
--   corrections land as new event/version rows, never UPDATEs of the
--   evidence ledger tables (venue_member_events, venue_case_evidence).

BEGIN;

-- ---------------------------------------------------------------------------
-- venue_members extensions — termination + jurisdiction (the register row
-- stays mutable state; history lives in venue_member_events)
-- ---------------------------------------------------------------------------

ALTER TABLE venue_members
    ADD COLUMN IF NOT EXISTS jurisdiction      VARCHAR(16),
    ADD COLUMN IF NOT EXISTS terminated_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS terminated_by     BIGINT,
    ADD COLUMN IF NOT EXISTS termination_reason VARCHAR(512);

-- ---------------------------------------------------------------------------
-- Member lifecycle ledger (immutable) + annual reviews
-- ---------------------------------------------------------------------------

CREATE TABLE venue_member_events (
    event_id    BIGSERIAL    PRIMARY KEY,
    member_id   BIGINT       NOT NULL REFERENCES venue_members (member_id),
    event_type  VARCHAR(24)  NOT NULL CHECK (event_type IN (
        'REGISTERED','DD_UPDATE','AGREEMENT','PRODUCTS','ADMISSION',
        'SUSPEND','REINSTATE','TERMINATE','APPEAL','APPEAL_DECISION',
        'REVIEW')),
    detail      JSONB        NOT NULL DEFAULT '{}'::jsonb,
    actor       BIGINT,                      -- admin user id
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX venue_member_events_member_ix
    ON venue_member_events (member_id, event_id);
COMMENT ON TABLE venue_member_events IS
    'Task 21.3.15 immutable member/DEA/sponsored lifecycle ledger (spec '
    '§14.1b): registration, due diligence, agreements, product/port '
    'approval, admission, suspension/reinstatement, termination, appeals '
    'and appeal decisions. APPEND-ONLY — the audit-grade evidence trail; '
    'audit_hash_chain rows anchor every append.';

CREATE TABLE venue_member_reviews (
    review_id       BIGSERIAL   PRIMARY KEY,
    member_id       BIGINT      NOT NULL REFERENCES venue_members (member_id),
    review_type     VARCHAR(12) NOT NULL DEFAULT 'ANNUAL'
                    CHECK (review_type IN ('ANNUAL','AD_HOC')),
    outcome         VARCHAR(12) NOT NULL
                    CHECK (outcome IN ('PASS','CONDITIONAL','FAIL')),
    findings        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    next_review_due DATE        NOT NULL,
    reviewer        BIGINT      NOT NULL,   -- Compliance Officer user id
    reviewed_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX venue_member_reviews_member_ix
    ON venue_member_reviews (member_id, reviewed_at DESC);
COMMENT ON TABLE venue_member_reviews IS
    'Task 21.3.15 annual/ad-hoc member risk reviews (spec §14.1b): '
    'outcome + findings + rolled next_review_due. A lapsed '
    'venue_members.annual_review_due pages ANNUAL_ATTESTATION_OVERDUE '
    '(P1) and blocks trading admission for the member until a review '
    'lands.';

-- ---------------------------------------------------------------------------
-- Rulebook / product governance (versioned)
-- ---------------------------------------------------------------------------

CREATE TABLE venue_rulebooks (
    rulebook_id    BIGSERIAL    PRIMARY KEY,
    kind           VARCHAR(16)  NOT NULL
                   CHECK (kind IN ('RULEBOOK','PRODUCT_TERMS')),
    scope_key      VARCHAR(64)  NOT NULL DEFAULT 'VENUE',  -- 'VENUE' |
                                -- instrument symbol | product type
    version        VARCHAR(32)  NOT NULL,
    body_ref       VARCHAR(256) NOT NULL,                  -- content-store ref
    status         VARCHAR(16)  NOT NULL DEFAULT 'DRAFT'
                   CHECK (status IN ('DRAFT','FILED','APPROVED',
                                     'EFFECTIVE','SUPERSEDED','WITHDRAWN')),
    requires_regulator_approval BOOLEAN NOT NULL DEFAULT false,
    regulator_status VARCHAR(12) NOT NULL DEFAULT 'NOT_REQUIRED'
                   CHECK (regulator_status IN
                          ('NOT_REQUIRED','FILED','APPROVED','REJECTED')),
    regulator_filing_ref VARCHAR(128),
    notice_period_days INTEGER  NOT NULL DEFAULT 10
                   CHECK (notice_period_days >= 0),
    emergency      BOOLEAN      NOT NULL DEFAULT false,   -- emergency rule
                                -- change: zero-notice activation, reason
                                -- recorded in detail (SDD edge case)
    detail         JSONB        NOT NULL DEFAULT '{}'::jsonb,
    effective_from TIMESTAMPTZ,
    approved_by    BIGINT,
    approved_at    TIMESTAMPTZ,
    filed_by       BIGINT,
    filed_at       TIMESTAMPTZ,
    activated_at   TIMESTAMPTZ,
    created_by     BIGINT       NOT NULL,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (kind, scope_key, version)
);
COMMENT ON TABLE venue_rulebooks IS
    'Task 21.3.15 versioned rulebook/product terms (spec §14.1b): no '
    'activation before required approvals — EFFECTIVE requires APPROVED '
    'status, regulator_status=APPROVED when requires_regulator_approval, '
    'and effective_from reached (emergency rows may activate immediately '
    'with the reason recorded). Superseded versions are retained.';

CREATE TABLE venue_rule_notices (
    notice_id   BIGSERIAL    PRIMARY KEY,
    rulebook_id BIGINT       NOT NULL REFERENCES venue_rulebooks (rulebook_id),
    subject     VARCHAR(256) NOT NULL,
    body_ref    VARCHAR(256),
    member_id   BIGINT       REFERENCES venue_members (member_id), -- NULL = all participants
    issued_by   BIGINT       NOT NULL,
    issued_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX venue_rule_notices_book_ix
    ON venue_rule_notices (rulebook_id, issued_at DESC);
COMMENT ON TABLE venue_rule_notices IS
    'Task 21.3.15 participant notices per rulebook version (spec §14.1b): '
    'member_id NULL broadcasts to all members; acknowledgements land in '
    'venue_rule_acks.';

CREATE TABLE venue_rule_acks (
    ack_id          BIGSERIAL    PRIMARY KEY,
    rulebook_id     BIGINT       NOT NULL REFERENCES venue_rulebooks (rulebook_id),
    notice_id       BIGINT       REFERENCES venue_rule_notices (notice_id),
    member_id       BIGINT       NOT NULL REFERENCES venue_members (member_id),
    acknowledged_by VARCHAR(128) NOT NULL,   -- member-side signatory
    evidence        JSONB        NOT NULL DEFAULT '{}'::jsonb,
    acknowledged_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    recorded_by     BIGINT       NOT NULL,
    UNIQUE (rulebook_id, member_id, notice_id)
);
COMMENT ON TABLE venue_rule_acks IS
    'Task 21.3.15 member acknowledgement evidence per rulebook/notice '
    '(spec §14.1b).';

-- ---------------------------------------------------------------------------
-- Real-time market control + emergency record
-- ---------------------------------------------------------------------------

CREATE TABLE venue_interventions (
    intervention_id BIGSERIAL    PRIMARY KEY,
    kind            VARCHAR(24)  NOT NULL CHECK (kind IN (
        'LIMIT','HALT','CANCELLATION','CORRECTION','INFO_REQUEST',
        'POSITION_ACCOUNTABILITY','EMERGENCY_RULE')),
    instrument_id   BIGINT,                  -- logical ref → instruments.id
    member_id       BIGINT       REFERENCES venue_members (member_id),
    account_id      BIGINT,
    status          VARCHAR(12)  NOT NULL DEFAULT 'ACTIVE'
                    CHECK (status IN ('ACTIVE','LIFTED','CLOSED')),
    reason          VARCHAR(1024) NOT NULL,
    detail          JSONB        NOT NULL DEFAULT '{}'::jsonb,
    imposed_by      BIGINT       NOT NULL,
    imposed_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    lifted_by       BIGINT,
    lifted_at       TIMESTAMPTZ
);
CREATE INDEX venue_interventions_open_ix
    ON venue_interventions (status, kind) WHERE status = 'ACTIVE';
COMMENT ON TABLE venue_interventions IS
    'Task 21.3.15 market-control + emergency record (spec §14.1b): '
    'limits, halts, cancellations/corrections, information requests, '
    'position accountability and emergency rules — complete evidence per '
    'action (immutable audit chain on every insert/transition).';

-- ---------------------------------------------------------------------------
-- Investigation / disciplinary cases + immutable evidence
-- ---------------------------------------------------------------------------

CREATE TABLE venue_cases (
    case_id     BIGSERIAL    PRIMARY KEY,
    case_ref    VARCHAR(32)  NOT NULL UNIQUE,      -- VC-<year>-<seq>
    kind        VARCHAR(16)  NOT NULL
                CHECK (kind IN ('INVESTIGATION','DISCIPLINARY')),
    member_id   BIGINT       REFERENCES venue_members (member_id),
    account_id  BIGINT,
    subject     VARCHAR(256) NOT NULL,
    status      VARCHAR(16)  NOT NULL DEFAULT 'OPEN'
                CHECK (status IN ('OPEN','INVESTIGATING','CHARGED',
                                  'SANCTIONED','DISMISSED','CLOSED')),
    outcome     VARCHAR(1024),
    detail      JSONB        NOT NULL DEFAULT '{}'::jsonb,
    opened_by   BIGINT       NOT NULL,
    opened_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    closed_by   BIGINT,
    closed_at   TIMESTAMPTZ
);
CREATE INDEX venue_cases_status_ix ON venue_cases (status, kind);
COMMENT ON TABLE venue_cases IS
    'Task 21.3.15 investigation/disciplinary case workflow (spec §14.1b): '
    'OPEN → INVESTIGATING → CHARGED → SANCTIONED|DISMISSED|CLOSED; '
    'evidence attachments are immutable (venue_case_evidence).';

CREATE TABLE venue_case_evidence (
    evidence_id  BIGSERIAL    PRIMARY KEY,
    case_id      BIGINT       NOT NULL REFERENCES venue_cases (case_id),
    evidence_ref VARCHAR(256) NOT NULL,
    sha256       CHAR(64),
    note         VARCHAR(512),
    attached_by  BIGINT       NOT NULL,
    attached_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX venue_case_evidence_case_ix
    ON venue_case_evidence (case_id, evidence_id);
COMMENT ON TABLE venue_case_evidence IS
    'Immutable evidence attachments for venue cases (spec §14.1b): '
    'sha256-pinned refs; rows are never updated or deleted.';

-- ---------------------------------------------------------------------------
-- Conflicts-of-interest register
-- ---------------------------------------------------------------------------

CREATE TABLE venue_conflicts (
    conflict_id     BIGSERIAL    PRIMARY KEY,
    member_id       BIGINT       REFERENCES venue_members (member_id),
    officer_user_id BIGINT,                          -- recusal axis
    subject         VARCHAR(256) NOT NULL,
    nature          VARCHAR(512) NOT NULL,
    status          VARCHAR(12)  NOT NULL DEFAULT 'DECLARED'
                    CHECK (status IN ('DECLARED','MITIGATED','RECUSED','CLOSED')),
    mitigation      VARCHAR(1024),
    declared_by     BIGINT       NOT NULL,
    declared_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    resolved_by     BIGINT,
    resolved_at     TIMESTAMPTZ
);
COMMENT ON TABLE venue_conflicts IS
    'Task 21.3.15 conflicts-of-interest register (spec §14.1b): DECLARED → '
    'MITIGATED/RECUSED/CLOSED — the conflict-of-interest recusal edge '
    'case is a first-class status, not a note.';

-- ---------------------------------------------------------------------------
-- Annual system-safeguard self-assessment + CCO report
-- ---------------------------------------------------------------------------

CREATE TABLE venue_self_assessments (
    assessment_id   BIGSERIAL    PRIMARY KEY,
    period_year     INTEGER      NOT NULL,
    version         INTEGER      NOT NULL DEFAULT 1,
    status          VARCHAR(12)  NOT NULL DEFAULT 'DRAFT'
                    CHECK (status IN ('DRAFT','COMPLETED')),
    evidence        JSONB        NOT NULL DEFAULT '{}'::jsonb, -- assembled
                                 -- control evidence snapshot
    exceptions      JSONB        NOT NULL DEFAULT '[]'::jsonb,
    financial_attestation JSONB  NOT NULL DEFAULT '{}'::jsonb,
    remediation     JSONB        NOT NULL DEFAULT '[]'::jsonb, -- unresolved
    assessed_by     BIGINT,
    assessed_at     TIMESTAMPTZ,
    created_by      BIGINT       NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (period_year, version)
);
COMMENT ON TABLE venue_self_assessments IS
    'Task 21.3.15 annual system-safeguard self-assessment (MiFID II RTS 7 '
    '/ CFTC SEF; spec §14.1b): assembled control evidence, exceptions, '
    'financial-resource attestation and unresolved remediation. New '
    'assessments for a period land as version+1 — history retained.';

CREATE TABLE cco_reports (
    id              BIGSERIAL    PRIMARY KEY,   -- board_pack probes max(id)
    period_start    DATE         NOT NULL,
    period_end      DATE         NOT NULL,
    version         INTEGER      NOT NULL DEFAULT 1,
    status          VARCHAR(12)  NOT NULL DEFAULT 'DRAFT'
                    CHECK (status IN ('DRAFT','SIGNED','FILED')),
    content         JSONB        NOT NULL DEFAULT '{}'::jsonb, -- assembled
                                 -- control evidence + statistics
    unresolved_remediation JSONB NOT NULL DEFAULT '[]'::jsonb,
    generated_by    BIGINT       NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    board_signed_by BIGINT,
    board_signed_at TIMESTAMPTZ,
    regulator_filing_ref VARCHAR(128),
    filed_by        BIGINT,
    filed_at        TIMESTAMPTZ,
    UNIQUE (period_start, period_end, version)
);
COMMENT ON TABLE cco_reports IS
    'Task 21.3.15 CCO annual report (spec §14.1b): assembled control '
    'evidence, exceptions and unresolved remediation with board and '
    'regulator sign-off tracking. Versioned per period; board pack '
    'section cco_report reads this table.';

-- ---------------------------------------------------------------------------
-- Launch prerequisites — the launch gate (AC: blocks when required
-- authorization/attestations are absent)
-- ---------------------------------------------------------------------------

CREATE TABLE venue_launch_prerequisites (
    prereq_id    BIGSERIAL    PRIMARY KEY,
    kind         VARCHAR(24)  NOT NULL CHECK (kind IN (
        'LICENSE','REGULATOR_AUTH','LEGAL_OPINION','BOARD_APPOINTMENT',
        'CCO_APPOINTMENT','MIN_FINANCIAL_RESOURCES')),
    scope        VARCHAR(16)  NOT NULL DEFAULT 'GLOBAL',  -- 'GLOBAL' or a
                 -- jurisdiction code (member-trading gate axis)
    required     BOOLEAN      NOT NULL DEFAULT true,
    status       VARCHAR(12)  NOT NULL DEFAULT 'MISSING'
                 CHECK (status IN ('MISSING','EVIDENCED','EXPIRED')),
    description  VARCHAR(512) NOT NULL,
    evidence_ref VARCHAR(256),
    evidenced_by BIGINT,
    evidenced_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    created_by   BIGINT       NOT NULL,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (kind, scope)
);
COMMENT ON TABLE venue_launch_prerequisites IS
    'Task 21.3.15 launch prerequisites (spec §14.1b — licensing, '
    'regulator authorization, legal opinions, board/CCO appointments and '
    'minimum financial resources are organizational prerequisites the '
    'system tracks, not claims it satisfies). GLOBAL scope rows gate '
    'production launch; LICENSE/REGULATOR_AUTH rows scoped to a '
    'jurisdiction gate member trading for members of that jurisdiction '
    '(JURISDICTION_UNLICENSED).';

-- Seed the GLOBAL required-prerequisite checklist — every row lands
-- MISSING so the launch gate blocks until an officer evidences each.
INSERT INTO venue_launch_prerequisites (kind, scope, required, status, description, created_by)
VALUES
    ('LICENSE',                'GLOBAL', true, 'MISSING', 'Venue operating license (MiFID II MTF/OTF, CFTC SEF or equivalent)', 0),
    ('REGULATOR_AUTH',         'GLOBAL', true, 'MISSING', 'Competent-authority venue authorization on file', 0),
    ('LEGAL_OPINION',          'GLOBAL', true, 'MISSING', 'External legal opinion covering venue rulebook enforceability', 0),
    ('BOARD_APPOINTMENT',      'GLOBAL', true, 'MISSING', 'Board appointment/minutes evidencing governance body', 0),
    ('CCO_APPOINTMENT',        'GLOBAL', true, 'MISSING', 'Chief Compliance Officer appointment record', 0),
    ('MIN_FINANCIAL_RESOURCES','GLOBAL', true, 'MISSING', 'Minimum financial resources attestation (prudential floor)', 0)
ON CONFLICT (kind, scope) DO NOTHING;

COMMIT;
