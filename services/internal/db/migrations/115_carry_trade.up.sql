-- 115_carry_trade.up.sql
-- Phase-03 Task 3.3.15 (spec §15.3, §24 #288): automated carry-trade swap
-- yield tracking and bot settlement.
--
--   carry_trade_allocations — one row per open carry-trade bot allocation
--     (the bots themselves are Phase-16's; bot_ref is the registry handle
--     and the balance lands on the bot's sub-account, account_id).
--   carry_trade_legs        — hedged position legs belonging to an
--     allocation (long high-yield leg + optional hedge legs).
--   carry_yield_records     — daily distribution audit: gross credits /
--     debits, net yield, per-currency running cumulative yield, and the
--     GL journal link.
--   carry_yield_totals      — running per-(allocation, currency) totals so
--     cumulative yield survives replays and feeds bot reporting.

BEGIN;

CREATE TYPE carry_allocation_status_enum AS ENUM ('ACTIVE', 'PAUSED', 'CLOSED');

CREATE TABLE carry_trade_allocations (
    id          BIGSERIAL PRIMARY KEY,
    account_id  BIGINT      NOT NULL REFERENCES accounts (id), -- bot sub-account receiving distributions
    bot_ref     VARCHAR(64) NOT NULL,                          -- Phase-16 bot registry handle
    status      carry_allocation_status_enum NOT NULL DEFAULT 'ACTIVE',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at   TIMESTAMPTZ
);
-- At most one ACTIVE allocation per bot handle.
CREATE UNIQUE INDEX carry_trade_allocations_active_ux
    ON carry_trade_allocations (bot_ref) WHERE status = 'ACTIVE';

CREATE TABLE carry_trade_legs (
    id            BIGSERIAL PRIMARY KEY,
    allocation_id BIGINT      NOT NULL REFERENCES carry_trade_allocations (id) ON DELETE CASCADE,
    position_id   BIGINT      NOT NULL REFERENCES positions (id),
    instrument_id BIGINT      NOT NULL REFERENCES instruments (id),
    side          swap_side_enum NOT NULL,                  -- enum created by migration 088
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (allocation_id, position_id)
);

CREATE TABLE carry_yield_records (
    id               BIGSERIAL PRIMARY KEY,
    allocation_id    BIGINT      NOT NULL REFERENCES carry_trade_allocations (id),
    journal_entry_id BIGINT      REFERENCES journal_entries (id), -- NULL when net = 0 (nothing posted)
    accrual_date     DATE        NOT NULL,
    currency         VARCHAR(3)  NOT NULL,
    days             INTEGER     NOT NULL CHECK (days >= 1),      -- rollover day multiplier applied
    gross_credit     DECIMAL(28,8) NOT NULL DEFAULT 0,            -- sum of positive leg deltas
    gross_debit      DECIMAL(28,8) NOT NULL DEFAULT 0,            -- sum of |negative leg deltas|
    net_yield        DECIMAL(28,8) NOT NULL,                      -- signed net across legs
    cumulative_yield DECIMAL(28,8) NOT NULL,                      -- running total for (allocation, currency)
    leg_detail       JSONB       NOT NULL DEFAULT '[]'::jsonb,    -- per-leg breakdown for bot reporting
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (allocation_id, accrual_date, currency)                -- replay-safe
);
CREATE INDEX carry_yield_records_ix ON carry_yield_records (allocation_id, accrual_date);

CREATE TABLE carry_yield_totals (
    allocation_id    BIGINT      NOT NULL REFERENCES carry_trade_allocations (id),
    currency         VARCHAR(3)  NOT NULL,
    cumulative_yield DECIMAL(28,8) NOT NULL DEFAULT 0,
    distributions    INTEGER     NOT NULL DEFAULT 0,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (allocation_id, currency)
);

COMMIT;
