-- 098_entity_leverage_policy.up.sql
-- Phase-19 Task 19.3.24 (spec §13.14, §24 #371).
--
-- Two artifacts:
--
--   1. accounts.entity_code — the operating-entity / jurisdiction-regime
--      label the leverage resolver reads (e.g. EU-ESMA, UK-FCA,
--      US-CFTC, INTL). Free-form VARCHAR rather than an enum: entity
--      onboarding is an admin/ops workflow, not a code deploy.
--      'INTL' is the strict-baseline default — an account whose row
--      predates entity assignment resolves against the INTL policy
--      rows, never an unbounded ceiling.
--
--   2. entity_leverage_policy — (entity_code × client_category ×
--      instrument_group) → max_leverage ceiling, effective-dated so a
--      policy change never rewrites history; resolution picks the
--      newest row whose effective_from <= now() (overlapping dates:
--      latest wins). Seed rows mirror the Task 19.3.2 ESMA/CFTC retail
--      caps — any higher ceiling (offshore, professional) is a
--      visible, dual-controlled row, never code (spec §13.14).
--
-- Resolution contract (risk.LeverageService): effective leverage =
-- min(entity policy, regulatory category cap, Task 19.3.17 tiered
-- band, instruments.max_leverage, account-chosen leverage). A missing
-- entity row fails closed to the strictest seed cap (10:1).

BEGIN;

ALTER TABLE accounts
    ADD COLUMN entity_code VARCHAR(32) NOT NULL DEFAULT 'INTL';

CREATE TABLE entity_leverage_policy (
    id               BIGSERIAL PRIMARY KEY,
    entity_code      VARCHAR(32) NOT NULL,
    client_category  client_category_enum NOT NULL,
    instrument_group VARCHAR(8)  NOT NULL
                     CHECK (instrument_group IN ('MAJOR','MINOR','EXOTIC')),
    max_leverage     INTEGER     NOT NULL CHECK (max_leverage > 0),
    effective_from   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by       BIGINT,               -- admin user id (dual-control maker)
    updated_by       BIGINT,               -- admin user id (dual-control approver)
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT entity_leverage_policy_cell_uq
        UNIQUE (entity_code, client_category, instrument_group, effective_from)
);

CREATE INDEX entity_leverage_policy_lookup_idx
    ON entity_leverage_policy (entity_code, client_category, instrument_group,
                               effective_from DESC);

-- Seed rows (spec §13.14: "seed rows mirror the Task 19.3.2 ESMA/CFTC
-- caps"). Retail caps per regime; PROFESSIONAL/ELIGIBLE_COUNTERPARTY
-- carry the venue-default negotiable ceilings — bilateral contracts
-- tighten per account through account_leverage (migration 231), never
-- below these ceilings.
INSERT INTO entity_leverage_policy
    (entity_code, client_category, instrument_group, max_leverage)
VALUES
    -- EU-ESMA retail: 30:1 major / 20:1 minor / 10:1 exotic (ESMA product
    -- intervention). UK-FCA mirrors the same intervention rules.
    ('EU-ESMA','RETAIL','MAJOR',30), ('EU-ESMA','RETAIL','MINOR',20), ('EU-ESMA','RETAIL','EXOTIC',10),
    ('UK-FCA','RETAIL','MAJOR',30),  ('UK-FCA','RETAIL','MINOR',20),  ('UK-FCA','RETAIL','EXOTIC',10),
    -- US-CFTC retail: 50:1 major / 20:1 minor (NFA rules); exotic pinned
    -- at the strictest baseline 10:1 (CFTC has no exotic schedule).
    ('US-CFTC','RETAIL','MAJOR',50), ('US-CFTC','RETAIL','MINOR',20), ('US-CFTC','RETAIL','EXOTIC',10),
    -- INTL baseline: strictest retail caps — the default for accounts
    -- without an entity assignment and the fail-closed reference.
    ('INTL','RETAIL','MAJOR',30),    ('INTL','RETAIL','MINOR',20),    ('INTL','RETAIL','EXOTIC',10),
    -- Professional/ECP negotiable ceilings (all entities). These are
    -- ceilings, not entitlements: the effective leverage still resolves
    -- through tier bands, instrument max and the account's chosen
    -- leverage (most-restrictive wins).
    ('EU-ESMA','PROFESSIONAL','MAJOR',200), ('EU-ESMA','PROFESSIONAL','MINOR',100), ('EU-ESMA','PROFESSIONAL','EXOTIC',50),
    ('EU-ESMA','ELIGIBLE_COUNTERPARTY','MAJOR',200), ('EU-ESMA','ELIGIBLE_COUNTERPARTY','MINOR',100), ('EU-ESMA','ELIGIBLE_COUNTERPARTY','EXOTIC',50),
    ('UK-FCA','PROFESSIONAL','MAJOR',200), ('UK-FCA','PROFESSIONAL','MINOR',100), ('UK-FCA','PROFESSIONAL','EXOTIC',50),
    ('UK-FCA','ELIGIBLE_COUNTERPARTY','MAJOR',200), ('UK-FCA','ELIGIBLE_COUNTERPARTY','MINOR',100), ('UK-FCA','ELIGIBLE_COUNTERPARTY','EXOTIC',50),
    ('US-CFTC','PROFESSIONAL','MAJOR',200), ('US-CFTC','PROFESSIONAL','MINOR',100), ('US-CFTC','PROFESSIONAL','EXOTIC',50),
    ('US-CFTC','ELIGIBLE_COUNTERPARTY','MAJOR',200), ('US-CFTC','ELIGIBLE_COUNTERPARTY','MINOR',100), ('US-CFTC','ELIGIBLE_COUNTERPARTY','EXOTIC',50),
    ('INTL','PROFESSIONAL','MAJOR',200), ('INTL','PROFESSIONAL','MINOR',100), ('INTL','PROFESSIONAL','EXOTIC',50),
    ('INTL','ELIGIBLE_COUNTERPARTY','MAJOR',200), ('INTL','ELIGIBLE_COUNTERPARTY','MINOR',100), ('INTL','ELIGIBLE_COUNTERPARTY','EXOTIC',50)
ON CONFLICT (entity_code, client_category, instrument_group, effective_from) DO NOTHING;

COMMIT;
