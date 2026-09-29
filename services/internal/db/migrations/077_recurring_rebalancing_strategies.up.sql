-- 077_recurring_rebalancing_strategies.up.sql
-- Phase-16 Task 16.3.21 — Recurring FX conversion, target-allocation
-- rebalancing and the approved strategy marketplace (spec §5.39, §24 #296).
--
-- Tables:
--   strategies           account-owned strategy records. kind drives the
--                        semantics: RECURRING_CONVERSION carries
--                        from/to ccy + amount + schedule + next_run_at;
--                        REBALANCE carries targets JSONB (currency→weight)
--                        + drift_band_pct. Funds are never pre-reserved
--                        beyond the single next run (§24 #296).
--   strategy_templates   marketplace rows — configuration JSONB ONLY,
--                        never executable code. Lattice:
--                        PENDING_APPROVAL → APPROVED | REJECTED → RETIRED.
--                        Instantiation copies config onto a new strategy;
--                        later template edits can never mutate live
--                        strategies (copy-by-value, template_id is audit).
--   strategy_runs        one row per scheduled slot / drift trigger:
--                        gating outcome (MARKET_CLOSED skip etc.), child
--                        order ids, per-run fees/spread cost and P&L.
--
-- Executions always route as firm CLOB orders through the orders pipeline
-- (ruling R14 — principal/RFQ conversion is out of scope).

BEGIN;

CREATE TYPE strategy_kind_enum AS ENUM ('RECURRING_CONVERSION', 'REBALANCE');
CREATE TYPE strategy_status_enum AS ENUM ('ACTIVE', 'PAUSED', 'CANCELLED');
CREATE TYPE strategy_template_status_enum AS ENUM (
    'PENDING_APPROVAL', 'APPROVED', 'REJECTED', 'RETIRED');
CREATE TYPE strategy_run_status_enum AS ENUM (
    'PENDING',    -- slot claimed, gates running
    'SUBMITTED',  -- child order(s) accepted by the order pipeline
    'COMPLETED',  -- all child orders terminal; costs settled
    'SKIPPED',    -- a gate declined the run (reason in skip_reason)
    'FAILED',     -- internal error — surfaced, never retried silently
    'CANCELLED'   -- strategy cancelled mid-run
);

CREATE TABLE strategies (
    strategy_id     BIGSERIAL PRIMARY KEY,
    account_id      BIGINT NOT NULL REFERENCES accounts (id),
    kind            strategy_kind_enum NOT NULL,
    label           VARCHAR(128) NOT NULL DEFAULT '',
    -- RECURRING_CONVERSION fields
    from_currency   VARCHAR(3),
    to_currency     VARCHAR(3),
    amount          DECIMAL(28,8) CHECK (amount IS NULL OR amount > 0),
    schedule        VARCHAR(8) CHECK (schedule IS NULL OR schedule IN
                        ('DAILY', 'WEEKLY', 'MONTHLY')),
    -- REBALANCE fields: targets maps currency → weight (sums to 1);
    -- drift_band_pct triggers a run when |actual−target| exceeds it.
    targets         JSONB,
    drift_band_pct  DECIMAL(9,4) CHECK (drift_band_pct IS NULL OR drift_band_pct > 0),
    -- marketplace provenance: set when instantiated from an approved
    -- template (configuration copied by value — never executable code).
    template_id     BIGINT,
    status          strategy_status_enum NOT NULL DEFAULT 'ACTIVE',
    next_run_at     TIMESTAMPTZ,
    last_run_at     TIMESTAMPTZ,
    -- run-level rollup (USD-valued where cross-currency)
    realized_pnl    DECIMAL(28,8) NOT NULL DEFAULT 0,
    total_fees      DECIMAL(28,8) NOT NULL DEFAULT 0,
    total_spread_cost DECIMAL(28,8) NOT NULL DEFAULT 0,
    total_notional  DECIMAL(28,8) NOT NULL DEFAULT 0,
    high_water_pnl  DECIMAL(28,8) NOT NULL DEFAULT 0,
    max_drawdown    DECIMAL(28,8) NOT NULL DEFAULT 0,
    run_count       INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- kind-field consistency enforced in schema: each kind requires its
    -- own config columns and must not carry the sibling's.
    CHECK ((kind = 'RECURRING_CONVERSION'
            AND from_currency IS NOT NULL AND to_currency IS NOT NULL
            AND amount IS NOT NULL AND schedule IS NOT NULL
            AND targets IS NULL AND drift_band_pct IS NULL)
        OR (kind = 'REBALANCE'
            AND targets IS NOT NULL AND drift_band_pct IS NOT NULL
            AND from_currency IS NULL AND to_currency IS NULL
            AND amount IS NULL AND schedule IS NULL))
);
CREATE INDEX strategies_due_ix ON strategies (next_run_at)
    WHERE status = 'ACTIVE' AND next_run_at IS NOT NULL;
CREATE INDEX strategies_account_ix ON strategies (account_id, created_at DESC);
CREATE INDEX strategies_rebalance_ix ON strategies (strategy_id)
    WHERE status = 'ACTIVE' AND kind = 'REBALANCE';
-- FK added after strategy_templates (forward reference).
CREATE TABLE strategy_templates (
    template_id  BIGSERIAL PRIMARY KEY,
    name         VARCHAR(128) NOT NULL,
    description  VARCHAR(1024) NOT NULL DEFAULT '',
    kind         strategy_kind_enum NOT NULL,
    -- Configuration ONLY: validated allowlist of scalar keys; executable
    -- payloads are rejected at the service layer and never stored.
    config       JSONB NOT NULL,
    status       strategy_template_status_enum NOT NULL DEFAULT 'PENDING_APPROVAL',
    publisher_account_id BIGINT NOT NULL REFERENCES accounts (id),
    approved_by  BIGINT,
    approved_at  TIMESTAMPTZ,
    rejected_at  TIMESTAMPTZ,
    reject_reason VARCHAR(255),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX strategy_templates_status_ix ON strategy_templates (status, created_at DESC);

ALTER TABLE strategies
    ADD CONSTRAINT strategies_template_fk
    FOREIGN KEY (template_id) REFERENCES strategy_templates (template_id);
-- One instantiation per (account, template) keeps replays idempotent;
-- instantiating the same template twice creates distinct rows by design
-- only when the caller intends it — the index does not prevent that;
-- it accelerates template → strategy lookups for reporting.
CREATE INDEX strategies_template_ix ON strategies (template_id) WHERE template_id IS NOT NULL;

CREATE TABLE strategy_runs (
    run_id         BIGSERIAL PRIMARY KEY,
    strategy_id    BIGINT NOT NULL REFERENCES strategies (strategy_id),
    account_id     BIGINT NOT NULL REFERENCES accounts (id),
    -- scheduled_for is the slot instant (next_run_at value) for recurring
    -- strategies and the drift-detection instant for rebalances.
    scheduled_for  TIMESTAMPTZ NOT NULL,
    kind           strategy_kind_enum NOT NULL,
    status         strategy_run_status_enum NOT NULL DEFAULT 'PENDING',
    -- truthful gate outcome when the run does not execute
    -- (MARKET_CLOSED | INSUFFICIENT_FUNDS | INSUFFICIENT_MARGIN |
    --  PRODUCT_NOT_PERMITTED | APPROPRIATENESS_* | SPREAD_* | ...)
    skip_reason    VARCHAR(64),
    -- child order ids submitted under this run (rebalances may carry
    -- several); detail JSONB records per-leg intent + outcome.
    order_ids      BIGINT[] NOT NULL DEFAULT '{}',
    legs           JSONB NOT NULL DEFAULT '[]',
    notional       DECIMAL(28,8) NOT NULL DEFAULT 0,
    currency       VARCHAR(3) NOT NULL DEFAULT 'USD',
    expected_value DECIMAL(28,8),
    executed_value DECIMAL(28,8),
    fees           DECIMAL(28,8) NOT NULL DEFAULT 0,
    spread_cost    DECIMAL(28,8) NOT NULL DEFAULT 0,
    realized_pnl   DECIMAL(28,8) NOT NULL DEFAULT 0,
    drawdown       DECIMAL(28,8) NOT NULL DEFAULT 0,
    started_at     TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Idempotent slot claiming: one run row per (strategy, slot). Retries
-- resume the same row; duplicate scheduler ticks cannot double-fire.
CREATE UNIQUE INDEX strategy_runs_slot_ux
    ON strategy_runs (strategy_id, scheduled_for);
-- At most one non-terminal run per strategy (a PAUSED/cancelled strategy
-- or a still-open run blocks new run creation).
CREATE UNIQUE INDEX strategy_runs_open_ux
    ON strategy_runs (strategy_id)
    WHERE status IN ('PENDING', 'SUBMITTED');
CREATE INDEX strategy_runs_strategy_ix ON strategy_runs (strategy_id, scheduled_for DESC);
CREATE INDEX strategy_runs_open_status_ix ON strategy_runs (status)
    WHERE status IN ('PENDING', 'SUBMITTED');

COMMIT;
