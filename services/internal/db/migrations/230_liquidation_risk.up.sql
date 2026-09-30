-- 230_liquidation_risk.up.sql
-- Phase-19 liquidation cluster (Tasks 19.3.3/19.3.4/19.3.9/19.3.14/
-- 19.3.19/19.3.20/19.3.22; spec §13.3–§13.6c, §13.11, §13.13, §24
-- #34/#35/#36/#94/#97/#133/#218/#269/#320/#360).
--
--   insurance_fund              — gains a UNIQUE currency index (mig 016
--                                 declared the row-per-currency table
--                                 without the uniqueness its upsert
--                                 contract requires).
--   insurance_fund_transactions — Task 19.3.14 item 4: every fund
--                                 drawdown/credit, linked to its journal.
--   margin_call_events          — the §13.3 deposit-window episode log
--                                 (15min window state, outcome, timing).
--   liquidation_events          — per-account force-order record consumed
--                                 by GET /api/v1/account/liquidations
--                                 (§13.13, §24 #360).
--   nbp_events                  — §13.6c regulatory NBP write-off log.
--   adl_directives              — durable ADL force-close outbox; the C++
--                                 consumer (Phase-19 sibling, wire contract
--                                 AdlForceCloseBody) dispatches and the Go
--                                 side reconciles ADL_FILL reports here.
--   insurance_fund_governance   — per-currency capitalization policy
--                                 (Task 19.3.14: target, regulatory floor,
--                                 replenishment fraction).
--   insurance_fund_adjustments  — dual-control manual injection/withdrawal
--                                 queue (Task 19.3.14 item 7, §8.2 four-eyes).

BEGIN;

-- 016 declared no uniqueness on currency; the service upserts per currency.
CREATE UNIQUE INDEX IF NOT EXISTS insurance_fund_currency_ux
    ON insurance_fund (currency);

CREATE TABLE insurance_fund_transactions (
    id               BIGSERIAL PRIMARY KEY,
    currency         VARCHAR(3)   NOT NULL,
    direction        VARCHAR(6)   NOT NULL CHECK (direction IN ('CREDIT','DEBIT')),
    amount           DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    reason           VARCHAR(32)  NOT NULL CHECK (reason IN (
                         'LIQUIDATION_PENALTY','AUCTION_DEFICIENCY','LP_REBATE',
                         'NBP_RESTITUTION','CAPITAL_INJECTION',
                         'RETAINED_EARNINGS_SWEEP','CONTINGENT_FACILITY',
                         'MANUAL_ADJUSTMENT')),
    reference_type   VARCHAR(32),             -- 'liquidation'|'auction'|'nbp'|'manual'|'sweep'
    reference_id     BIGINT,
    account_id       BIGINT,                  -- counterparty account when a wallet moved
    journal_entry_id BIGINT       REFERENCES journal_entries (id),
    balance_after    DECIMAL(28,8) NOT NULL,  -- fund balance post-movement
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX insurance_fund_tx_ccy_idx  ON insurance_fund_transactions (currency, id DESC);
CREATE INDEX insurance_fund_tx_ref_idx  ON insurance_fund_transactions (reference_type, reference_id);

CREATE TABLE margin_call_events (
    id               BIGSERIAL PRIMARY KEY,
    account_id       BIGINT       NOT NULL REFERENCES accounts (id),
    margin_level_pct DECIMAL(12,4) NOT NULL,
    threshold_pct    DECIMAL(12,4) NOT NULL,    -- 111.1 canonical (§13.3)
    status           VARCHAR(12)  NOT NULL DEFAULT 'OPEN'
                     CHECK (status IN ('OPEN','RESTORED','LIQUIDATED','STOP_OUT')),
    notified_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at       TIMESTAMPTZ  NOT NULL,     -- deposit window end (15min)
    resolved_at      TIMESTAMPTZ,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now()
);
-- At most one open margin-call episode per account — the service resolves
-- before reopening; the partial unique index makes re-entry idempotent.
CREATE UNIQUE INDEX margin_call_events_open_ux
    ON margin_call_events (account_id) WHERE status = 'OPEN';
CREATE INDEX margin_call_events_expiry_idx
    ON margin_call_events (expires_at) WHERE status = 'OPEN';

CREATE TABLE liquidation_events (
    id                          BIGSERIAL PRIMARY KEY,
    account_id                  BIGINT      NOT NULL REFERENCES accounts (id),
    position_id                 BIGINT      NOT NULL,
    instrument_id               BIGINT      NOT NULL,
    auction_id                  BIGINT      REFERENCES liquidation_auctions (id),
    margin_call_event_id        BIGINT      REFERENCES margin_call_events (id),
    kind                        VARCHAR(16) NOT NULL
                                CHECK (kind IN ('DIRECT_CLOSE','AUCTION_FILL','FORCE_CASH','ADL')),
    side                        position_side_enum NOT NULL,  -- liquidated position side
    quantity                    DECIMAL(28,8) NOT NULL,
    price                       DECIMAL(20,8) NOT NULL,          -- realized close price
    mark_price                  DECIMAL(20,8),
    insurance_fund_contribution DECIMAL(28,8) NOT NULL DEFAULT 0, -- fund in/out on this leg
    penalty_amount              DECIMAL(28,8) NOT NULL DEFAULT 0,
    adl_quintile                SMALLINT    CHECK (adl_quintile IS NULL OR adl_quintile BETWEEN 1 AND 5),
    journal_entry_id            BIGINT      REFERENCES journal_entries (id),
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX liquidation_events_account_idx
    ON liquidation_events (account_id, created_at DESC, id DESC);
CREATE INDEX liquidation_events_instrument_idx
    ON liquidation_events (instrument_id, created_at DESC);

CREATE TABLE nbp_events (
    id               BIGSERIAL PRIMARY KEY,
    account_id       BIGINT       NOT NULL REFERENCES accounts (id),
    currency         VARCHAR(3)   NOT NULL,
    shortfall        DECIMAL(28,8) NOT NULL CHECK (shortfall > 0), -- deficit absorbed
    funding_source   VARCHAR(16)  NOT NULL CHECK (funding_source IN ('INSURANCE_FUND','HOUSE_PNL')),
    journal_entry_id BIGINT       REFERENCES journal_entries (id),
    status           VARCHAR(12)  NOT NULL DEFAULT 'POSTED'
                     CHECK (status IN ('POSTED','FAILED')),
    deficit_equity   DECIMAL(28,8),          -- account equity at trigger (base ccy)
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX nbp_events_account_idx ON nbp_events (account_id, created_at DESC);

CREATE TABLE adl_directives (
    adl_seq           BIGSERIAL PRIMARY KEY,          -- wire contract adl_seq
    account_id        BIGINT      NOT NULL REFERENCES accounts (id),
    instrument_id     BIGINT      NOT NULL,
    symbol            VARCHAR(32) NOT NULL,
    side              position_side_enum NOT NULL,    -- position side force-closed
    qty               DECIMAL(28,8) NOT NULL CHECK (qty > 0),
    bankruptcy_price  DECIMAL(20,8) NOT NULL,         -- counterparty fills at this price
    score             DECIMAL(28,8) NOT NULL,         -- profit_pct × effective_leverage
    quintile          SMALLINT    NOT NULL CHECK (quintile BETWEEN 1 AND 5),
    status            VARCHAR(12) NOT NULL DEFAULT 'QUEUED'
                      CHECK (status IN ('QUEUED','DISPATCHED','FILLED','FAILED')),
    liquidation_event_id BIGINT   REFERENCES liquidation_events (id),
    dispatched_at     TIMESTAMPTZ,
    filled_at         TIMESTAMPTZ,
    fill_price        DECIMAL(20,8),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX adl_directives_status_idx ON adl_directives (status) WHERE status IN ('QUEUED','DISPATCHED');
CREATE INDEX adl_directives_account_idx ON adl_directives (account_id, created_at DESC);

CREATE TABLE insurance_fund_governance (
    currency                VARCHAR(3) PRIMARY KEY,
    min_capital             DECIMAL(28,8) NOT NULL DEFAULT 10000000,  -- $10M floor (Task 19.3.14.1)
    target_pct_of_equity    DECIMAL(9,6)  NOT NULL DEFAULT 0.005,     -- 0.5% of client equity
    target_balance          DECIMAL(28,8),                            -- last computed target
    replenishment_fee_pct   DECIMAL(9,6)  NOT NULL DEFAULT 0.10,      -- 10% of daily fee revenue
    regulatory_floor        DECIMAL(28,8) NOT NULL DEFAULT 0,         -- per-jurisdiction minimum
    contingent_facility_cap DECIMAL(28,8) NOT NULL DEFAULT 0,         -- committed facility ceiling
    last_target_computed_at TIMESTAMPTZ,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO insurance_fund_governance (currency) VALUES ('USD')
ON CONFLICT (currency) DO NOTHING;

CREATE TABLE insurance_fund_adjustments (
    id               BIGSERIAL PRIMARY KEY,
    currency         VARCHAR(3)   NOT NULL,
    direction        VARCHAR(6)   NOT NULL CHECK (direction IN ('CREDIT','DEBIT')),
    amount           DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    reason           VARCHAR(255) NOT NULL,
    status           VARCHAR(16)  NOT NULL DEFAULT 'PENDING_APPROVAL'
                     CHECK (status IN ('PENDING_APPROVAL','EXECUTED','REJECTED')),
    initiated_by     BIGINT       NOT NULL,
    approved_by      BIGINT,
    journal_entry_id BIGINT       REFERENCES journal_entries (id),
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    -- §8.2 four-eyes: an executed adjustment needs a distinct approver.
    CONSTRAINT insurance_fund_adj_dual CHECK (approved_by IS NULL OR approved_by <> initiated_by)
);

COMMIT;
