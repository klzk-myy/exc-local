-- 082_treasury.up.sql
-- Phase-24 Task 24.3.17 — venue treasury, own funds & contingent-capital
-- backstop (spec §17.13.1, §24 #329).
--
-- own_funds_balances is the house balance sheet that stands BEHIND client
-- money: house equity, capital reserves and the insurance-fund balance as
-- first-class lines, distinct from client money (§17.9) and from the GL
-- (§17.1), reconciled daily against bank statements.

BEGIN;

CREATE TABLE own_funds_balances (
    id          BIGSERIAL PRIMARY KEY,
    line_kind   VARCHAR(20) NOT NULL
        CHECK (line_kind IN ('HOUSE_EQUITY','CAPITAL_RESERVE','INSURANCE_FUND','RETAINED_EARNINGS')),
    currency    VARCHAR(3) NOT NULL,
    balance     DECIMAL(28,8) NOT NULL DEFAULT 0,
    -- Daily reconciliation against bank statements (Task 24.3.12 seam):
    -- statement_ref identifies the source statement row/file used.
    reconciliation_status VARCHAR(12) NOT NULL DEFAULT 'PENDING'
        CHECK (reconciliation_status IN ('PENDING','RECONCILED','BREAK')),
    statement_ref         VARCHAR(64),
    statement_balance     DECIMAL(28,8),
    reconciled_at         TIMESTAMPTZ,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (line_kind, currency)
);

-- Contingent-capital commitments — the insurance-fund funding waterfall:
-- house capital first (priority_seq ascending), then committed backstops
-- (sponsor, credit facility, insurer). The depletion sequence must terminate
-- in an EXECUTED, funded backstop — never an unbacked deficit (spec §17.13.1.2).
-- commitment_kind='INSURANCE_POLICY' rows carry business-interruption / cyber /
-- key-person / E&O cover with expiry, limit, excess and broker (item 3).
CREATE TABLE contingent_capital_commitments (
    id               BIGSERIAL PRIMARY KEY,
    provider_name    VARCHAR(128) NOT NULL,
    commitment_kind  VARCHAR(20) NOT NULL
        CHECK (commitment_kind IN ('HOUSE_CAPITAL','SPONSOR','CREDIT_FACILITY','INSURER','INSURANCE_POLICY')),
    priority_seq     INT NOT NULL,               -- waterfall draw order
    committed_amount DECIMAL(28,8) NOT NULL CHECK (committed_amount >= 0),
    drawn_amount     DECIMAL(28,8) NOT NULL DEFAULT 0 CHECK (drawn_amount >= 0),
    currency         VARCHAR(3) NOT NULL,
    activation_trigger VARCHAR(64) NOT NULL,     -- e.g. 'INSURANCE_FUND_EXHAUSTED'
    draw_window_days INT NOT NULL DEFAULT 0 CHECK (draw_window_days >= 0),
    agreement_ref    VARCHAR(128) NOT NULL,      -- governing-agreement reference
    -- insurance-cover columns (commitment_kind='INSURANCE_POLICY')
    policy_type      VARCHAR(24)                 -- BUSINESS_INTERRUPTION|CYBER|KEY_PERSON|ERRORS_OMISSIONS
        CHECK (policy_type IS NULL OR policy_type IN
               ('BUSINESS_INTERRUPTION','CYBER','KEY_PERSON','ERRORS_OMISSIONS')),
    cover_limit      DECIMAL(28,8),
    excess           DECIMAL(28,8),
    broker           VARCHAR(128),
    expires_at       TIMESTAMPTZ,                -- expiry inside 60d → INSURANCE_POLICY_EXPIRING (P2)
    status           VARCHAR(12) NOT NULL DEFAULT 'COMMITTED'
        CHECK (status IN ('COMMITTED','EXECUTED','DRAWN','LAPSED','EXPIRED')),
    executed_at      TIMESTAMPTZ,                -- agreement executed/funded
    expiry_alerted_at TIMESTAMPTZ,               -- INSURANCE_POLICY_EXPIRING dedup
    created_by       BIGINT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (drawn_amount <= committed_amount)
);
CREATE INDEX ccc_waterfall_ix ON contingent_capital_commitments (priority_seq, status);

-- Stressed 5-business-day liquidity assessments (item 4) — every evaluation
-- lands a row; a breach sets treasury_controls flags in the same tx.
CREATE TABLE treasury_liquidity_assessments (
    id                   BIGSERIAL PRIMARY KEY,
    assessed_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    currency             VARCHAR(3) NOT NULL,
    liquid_house_funds   DECIMAL(28,8) NOT NULL,
    stressed_outflow_5d  DECIMAL(28,8) NOT NULL, -- forced withdrawals + adverse rollover + fund call
    required_buffer      DECIMAL(28,8) NOT NULL, -- ratio × stressed_outflow_5d
    status               VARCHAR(8) NOT NULL CHECK (status IN ('OK','BREACH')),
    detail               JSONB,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Single-row control flags — a liquidity breach freezes discretionary house
-- outflows and blocks new LP capacity until remediated (spec §17.13.1.4).
CREATE TABLE treasury_controls (
    id                              SMALLINT PRIMARY KEY CHECK (id = 1),
    discretionary_outflows_frozen   BOOLEAN NOT NULL DEFAULT FALSE,
    lp_capacity_blocked             BOOLEAN NOT NULL DEFAULT FALSE,
    frozen_at                       TIMESTAMPTZ,
    freeze_reason                   VARCHAR(64),
    unfrozen_by                     BIGINT,
    unfrozen_at                     TIMESTAMPTZ
);
INSERT INTO treasury_controls (id) VALUES (1);

COMMIT;
