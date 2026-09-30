-- 259_cls_pvp
-- Phase-24 Task 24.3.8 — Continuous Linked Settlement (CLS) third-party
-- PvP settlement service (spec §17.6, §24 #126).
--
-- Three table families:
--
--   1. cls_reference_versions / cls_reference_entries — VERSIONED CLS
--      reference data. Eligible currencies, products, settlement members,
--      cut-off windows (initial pay-in / final pay-in / rescind deadline),
--      alternative-PvP availability and controlled-gross principal-risk
--      limits all live here. Nothing is hard-coded: the eligibility
--      checker and cut-off gates read the ACTIVE version only (Task
--      24.3.8 step 1 — the superseded hard-coded 18-currency list and
--      06:30/09:00 CET constants are deliberately absent).
--
--   2. cls_settlement_instructions — one row per paired PvP instruction
--      (buy leg + sell leg) with the full persisted status lifecycle:
--      RECEIVED → VALIDATED → MATCHED|UNMATCHED → ELIGIBLE|INELIGIBLE →
--      PAY_IN → SETTLED | RESCINDED | EXPIRED | REJECTED.
--
--   3. cls_instruction_events — append-only status-transition journal
--      carrying the authenticated-finality flag (GL/nostro finality posts
--      ONLY from an authenticated member finality event).

BEGIN;

CREATE TYPE cls_reference_status_enum AS ENUM ('DRAFT', 'ACTIVE', 'RETIRED');

CREATE TABLE cls_reference_versions (
    id             BIGSERIAL PRIMARY KEY,
    version        VARCHAR(32) NOT NULL UNIQUE,     -- e.g. 'CLS-REF-2026-10'
    status         cls_reference_status_enum NOT NULL DEFAULT 'DRAFT',
    effective_from TIMESTAMPTZ NOT NULL,
    activated_at   TIMESTAMPTZ,
    retired_at     TIMESTAMPTZ,
    notes          TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one ACTIVE reference-data version at a time.
CREATE UNIQUE INDEX cls_reference_one_active_ux
    ON cls_reference_versions (status) WHERE status = 'ACTIVE';

CREATE TABLE cls_reference_entries (
    id          BIGSERIAL PRIMARY KEY,
    version_id  BIGINT      NOT NULL REFERENCES cls_reference_versions (id),
    entry_type  VARCHAR(24) NOT NULL CHECK (entry_type IN
        ('CURRENCY', 'PRODUCT', 'MEMBER', 'CUTOFF', 'ALT_PVP', 'PRINCIPAL_LIMIT')),
    entry_key   VARCHAR(64) NOT NULL,               -- currency / product / member BIC / cutoff name / pair
    payload     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (version_id, entry_type, entry_key)
);

CREATE INDEX cls_reference_entries_ix
    ON cls_reference_entries (version_id, entry_type);

CREATE TYPE cls_instruction_status_enum AS ENUM
    ('RECEIVED', 'VALIDATED', 'MATCHED', 'UNMATCHED', 'ELIGIBLE',
     'INELIGIBLE', 'PAY_IN', 'SETTLED', 'RESCINDED', 'EXPIRED', 'REJECTED');

-- Settlement-risk waterfall route for CLS-ineligible flow (spec §17.6
-- step 5): alternative PvP → bilateral netting → controlled gross.
CREATE TYPE cls_settlement_route_enum AS ENUM
    ('CLS_PVP', 'ALT_PVP', 'NETTING', 'CONTROLLED_GROSS');

CREATE TABLE cls_settlement_instructions (
    id                      BIGSERIAL PRIMARY KEY,
    instruction_ref         VARCHAR(32)  NOT NULL UNIQUE, -- our correlation ref (MsgId / :20:)
    counterparty_account_id BIGINT       NOT NULL REFERENCES accounts (id),
    member_bic              VARCHAR(11)  NOT NULL,        -- CLS settlement member we instruct through
    product                 VARCHAR(16)  NOT NULL,        -- SPOT | FORWARD | SWAP | NDF ...
    buy_currency            VARCHAR(3)   NOT NULL,
    buy_amount              DECIMAL(28,8) NOT NULL CHECK (buy_amount > 0),
    sell_currency           VARCHAR(3)   NOT NULL,
    sell_amount             DECIMAL(28,8) NOT NULL CHECK (sell_amount > 0),
    value_date              DATE         NOT NULL,
    trade_id                BIGINT,                        -- source trade (nullable — manual instructions)
    status                  cls_instruction_status_enum NOT NULL DEFAULT 'RECEIVED',
    settlement_route        cls_settlement_route_enum NOT NULL DEFAULT 'CLS_PVP',
    member_instruction_id   VARCHAR(64),                   -- CLS member-side instruction id
    member_ack_ref          VARCHAR(64),
    uetr                    VARCHAR(36),
    message_payload         TEXT,                          -- ISO 20022 XML body
    payload_version         INTEGER      NOT NULL DEFAULT 0, -- amend counter
    rescinded_reason        TEXT,
    reject_reason           TEXT,
    ref_version_id          BIGINT REFERENCES cls_reference_versions (id),
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    settled_at              TIMESTAMPTZ,
    gl_journal_id           BIGINT                         -- finality GL posting reference
);

CREATE INDEX cls_instructions_status_ix
    ON cls_settlement_instructions (status, value_date);
CREATE INDEX cls_instructions_member_ix
    ON cls_settlement_instructions (member_instruction_id)
    WHERE member_instruction_id IS NOT NULL;

CREATE TABLE cls_instruction_events (
    id              BIGSERIAL PRIMARY KEY,
    instruction_id  BIGINT NOT NULL REFERENCES cls_settlement_instructions (id),
    from_status     cls_instruction_status_enum,
    to_status       cls_instruction_status_enum NOT NULL,
    -- TRUE only for an authenticated member finality notification — the
    -- sole trigger allowed to flip a PAY_IN instruction to SETTLED and
    -- release the GL/nostro finality posting.
    authenticated   BOOLEAN NOT NULL DEFAULT FALSE,
    member_ref      VARCHAR(64),
    detail          TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX cls_events_instruction_ix ON cls_instruction_events (instruction_id, id);

COMMIT;
