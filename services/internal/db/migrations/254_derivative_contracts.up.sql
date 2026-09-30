-- 254_derivative_contracts.up.sql
-- Phase-22 Tasks 22.3.1/22.3.2/22.3.3 (spec §5.4 dated params, §6.3,
-- §15.1, §15.7; §24 #57/#58/#59/#396).
--
-- Three pieces:
--
--   1. derivative_contracts — the booked forward/swap/NDF record (one row
--      per account per fill). Rates are quote-per-base; notional is the
--      base-currency amount exchanged per leg. Swap legs ride
--      near_leg_value_date + value_date (far); the NDF fields carry the
--      §15.7 fixing-date/source/settlement vocabulary plus the fixing
--      rate and signed cash amount once settled. trade_id is a logical
--      ref to trades.id — no declarative FK (trades is daily-
--      partitioned; same documented discipline as settlement_
--      instructions, migration 019 header note).
--
--   2. ndf_fixings — one fixing observation per (contract, fixing_date),
--      carrying the §15.7 source tier and the prior-day-hold flag.
--
--   3. settlement_instructions gains derivative_contract_id + leg_tag —
--      the dated currency obligations (forward delivery legs, swap
--      near/far legs, NDF cash leg) flow through the existing Phase-03
--      dispatch/reconciliation pipeline unchanged; the link column lets
--      the contract rollup see leg state. Additive NULL columns, no
--      change to existing rows or the leg_ux unique index.

BEGIN;

CREATE TYPE derivative_kind_enum AS ENUM ('FORWARD', 'SWAP', 'NDF');
CREATE TYPE derivative_contract_status_enum AS ENUM
    ('OPEN', 'PARTIALLY_SETTLED', 'SETTLED', 'FAILED', 'CANCELLED');
CREATE TYPE derivative_leg_tag_enum AS ENUM ('NEAR', 'FAR', 'FIXING');

CREATE TABLE derivative_contracts (
    id                   BIGSERIAL PRIMARY KEY,
    trade_id             BIGINT NOT NULL,               -- logical ref → trades.id (see header)
    account_id           BIGINT NOT NULL REFERENCES accounts (id),
    instrument_id        BIGINT NOT NULL REFERENCES instruments (id),
    kind                 derivative_kind_enum NOT NULL,
    side                 VARCHAR(4) NOT NULL CHECK (side IN ('BUY','SELL')),
    base_currency        VARCHAR(3) NOT NULL,
    quote_currency       VARCHAR(3) NOT NULL,
    notional             DECIMAL(28,8) NOT NULL CHECK (notional > 0),
    spot_rate            DECIMAL(20,8) NOT NULL CHECK (spot_rate > 0),
    forward_rate         DECIMAL(20,8) NOT NULL CHECK (forward_rate > 0),
    swap_points          DECIMAL(20,8) NOT NULL,        -- forward_rate − spot_rate
    spot_value_date      DATE NOT NULL,                 -- pair spot value date (swap near leg basis)
    value_date           DATE NOT NULL,                 -- forward maturity / swap far leg / NDF fixing-settle
    near_leg_value_date  DATE,                          -- swap near leg (NULL for FORWARD/NDF)
    ndf_fixing_date      DATE,                          -- rolled fixing date (NDF only)
    ndf_fixing_source    VARCHAR(64),                   -- §15.7 tier[:page] vocabulary
    settlement_currency  VARCHAR(3),                    -- NDF cash-leg currency (deliverable side)
    fixing_rate          DECIMAL(20,8),                 -- set by ApplyFixing
    settlement_amount    DECIMAL(28,8),                 -- signed, settlement_currency
    status               derivative_contract_status_enum NOT NULL DEFAULT 'OPEN',
    booked_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at           TIMESTAMPTZ,
    idempotency_key      TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (kind <> 'SWAP'  OR near_leg_value_date IS NOT NULL),
    CHECK (kind <> 'NDF'   OR (ndf_fixing_date IS NOT NULL
                               AND ndf_fixing_source IS NOT NULL
                               AND settlement_currency IS NOT NULL)),
    CHECK (kind <> 'SWAP'  OR near_leg_value_date < value_date),
    CHECK (kind <> 'NDF'   OR ndf_fixing_date = value_date)
);

-- Fill-replay dedup: booking with a replayed key returns the existing row.
CREATE UNIQUE INDEX derivative_contracts_idem_ux
    ON derivative_contracts (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- Maturity/settlement sweep: due dated contracts.
CREATE INDEX derivative_contracts_due_ix
    ON derivative_contracts (value_date)
    WHERE status IN ('OPEN', 'PARTIALLY_SETTLED');
CREATE INDEX derivative_contracts_near_due_ix
    ON derivative_contracts (near_leg_value_date)
    WHERE status IN ('OPEN', 'PARTIALLY_SETTLED') AND near_leg_value_date IS NOT NULL;

CREATE TABLE ndf_fixings (
    id             BIGSERIAL PRIMARY KEY,
    contract_id    BIGINT NOT NULL REFERENCES derivative_contracts (id) ON DELETE CASCADE,
    fixing_date    DATE NOT NULL,
    source         VARCHAR(64) NOT NULL,                -- CENTRAL_BANK|REUTERS|BLOOMBERG|PRIOR_DAY_HOLD[:page]
    rate           DECIMAL(20,8) NOT NULL CHECK (rate > 0),
    prior_day_hold BOOLEAN NOT NULL DEFAULT FALSE,      -- §15.7 last-resort tier flag
    observed_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (contract_id, fixing_date)
);

-- Link derivative legs into the existing settlement-instruction pipeline.
ALTER TABLE settlement_instructions
    ADD COLUMN IF NOT EXISTS derivative_contract_id BIGINT
        REFERENCES derivative_contracts (id),
    ADD COLUMN IF NOT EXISTS leg_tag derivative_leg_tag_enum;

CREATE INDEX settlement_instructions_contract_ix
    ON settlement_instructions (derivative_contract_id)
    WHERE derivative_contract_id IS NOT NULL;

COMMIT;
