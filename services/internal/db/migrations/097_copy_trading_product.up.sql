-- 097_copy_trading_product.up.sql
-- Phase-14 Task 14.3.14 — Copy-Trading Product Layer (spec §12.9, §24 #372).
--
-- Tables:
--   strategy_profiles        manager-facing strategy records; status lattice
--                            INCUBATING → LISTED → SUSPENDED (LISTED requires
--                            ≥30 days incubating + appropriateness PASS —
--                            enforced in code, spec §12.9 item 1).
--   copy_follows             investor binding to a strategy: allocation
--                            notional, safety_mode (FULL|HALF_RISK — HALF_RISK
--                            scales child quantities ×0.5 BEFORE the
--                            min-notional check), investor stop-loss cap.
--   copy_child_orders        per-follow order intents derived pro-rata from
--                            master fills; SKIPPED_MIN_NOTIONAL rows carry a
--                            notice — children are never silently dropped.
--   high_water_marks         ratchet-only per-follow profit-share baseline;
--                            raised on payout, NEVER reset on loss months.
--   profit_share_accruals    month-end/unfollow accrual ledger (balanced GL
--                            journal reference via journal_entry_id).
--
-- The PAMM engine tables (pamm_pools / pamm_allocations /
-- pamm_subledger_entries / pamm_fill_allocations) land in migration 216 —
-- copy_follows is created here so 216 can FK the shared internal-investment
-- sub-ledger to follows as well as pools.

BEGIN;

CREATE TYPE copy_strategy_status_enum AS ENUM ('INCUBATING', 'LISTED', 'SUSPENDED');
CREATE TYPE copy_safety_mode_enum    AS ENUM ('FULL', 'HALF_RISK');
CREATE TYPE copy_follow_status_enum  AS ENUM ('ACTIVE', 'UNFOLLOWED', 'STOPPED');
CREATE TYPE copy_child_status_enum   AS ENUM (
    'PENDING',                 -- intent recorded, dispatch in flight
    'SUBMITTED',               -- child order accepted by the order pipeline
    'SKIPPED_MIN_NOTIONAL',    -- below min-notional after safety scaling
    'CANCELLED',               -- cancelled by unfollow before dispatch/fill
    'REJECTED'                 -- order pipeline rejected the child
);
CREATE TYPE profit_share_status_enum AS ENUM ('ACCRUED', 'PAID', 'VOID');

CREATE TABLE strategy_profiles (
    strategy_id        BIGSERIAL PRIMARY KEY,
    manager_account_id BIGINT NOT NULL REFERENCES accounts (id),
    display_name       VARCHAR(128) NOT NULL,
    description        VARCHAR(1024) NOT NULL DEFAULT '',
    currency           VARCHAR(3)   NOT NULL,          -- stats/AUM denomination
    -- instrument_class feeds the LISTED appropriateness gate (Task 14.3.7):
    -- SPOT passes trivially (unleveraged exempt), gated classes require a
    -- live PASS assessment for the manager account.
    instrument_class   VARCHAR(16)  NOT NULL DEFAULT 'SPOT'
        CHECK (instrument_class IN ('SPOT', 'FORWARD', 'SWAP', 'NDF', 'OPTION')),
    status             copy_strategy_status_enum NOT NULL DEFAULT 'INCUBATING',
    -- manager-set, admin-capped: the 50% ceiling is a hard DB CHECK —
    -- application code can lower it per operator policy, never raise it.
    profit_share_pct   DECIMAL(6,4) NOT NULL DEFAULT 0
        CHECK (profit_share_pct >= 0 AND profit_share_pct <= 50),
    incubating_since   TIMESTAMPTZ NOT NULL DEFAULT now(),
    listed_at          TIMESTAMPTZ,
    suspended_at       TIMESTAMPTZ,
    suspend_reason     VARCHAR(255),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX strategy_profiles_status_ix ON strategy_profiles (status);
CREATE INDEX strategy_profiles_mgr_ix    ON strategy_profiles (manager_account_id);

CREATE TABLE copy_follows (
    follow_id           BIGSERIAL PRIMARY KEY,
    investor_account_id BIGINT NOT NULL REFERENCES accounts (id),
    strategy_id         BIGINT NOT NULL REFERENCES strategy_profiles (strategy_id),
    allocation_notional DECIMAL(28,8) NOT NULL CHECK (allocation_notional > 0),
    currency            VARCHAR(3)    NOT NULL,        -- must equal strategy currency
    safety_mode         copy_safety_mode_enum NOT NULL DEFAULT 'FULL',
    stop_loss_cap       DECIMAL(28,8) CHECK (stop_loss_cap IS NULL OR stop_loss_cap > 0),
    status              copy_follow_status_enum NOT NULL DEFAULT 'ACTIVE',
    unfollowed_at       TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- One ACTIVE follow per (investor, strategy); history rows coexist.
CREATE UNIQUE INDEX copy_follows_active_ux
    ON copy_follows (investor_account_id, strategy_id) WHERE status = 'ACTIVE';
CREATE INDEX copy_follows_strategy_ix ON copy_follows (strategy_id) WHERE status = 'ACTIVE';
CREATE INDEX copy_follows_investor_ix ON copy_follows (investor_account_id, status);

CREATE TABLE copy_child_orders (
    child_id        BIGSERIAL PRIMARY KEY,
    follow_id       BIGINT NOT NULL REFERENCES copy_follows (follow_id),
    master_trade_id BIGINT NOT NULL,
    instrument_id   BIGINT NOT NULL,
    side            VARCHAR(4) NOT NULL CHECK (side IN ('BUY', 'SELL')),
    quantity        DECIMAL(28,8) NOT NULL CHECK (quantity > 0),
    master_price    DECIMAL(28,8) NOT NULL,
    status          copy_child_status_enum NOT NULL DEFAULT 'PENDING',
    child_order_id  BIGINT,                        -- orders.id once dispatched
    notice          VARCHAR(255),                  -- skip reason / investor notice
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Replay-safe: a re-delivered master fill maps to the same children.
    UNIQUE (master_trade_id, follow_id)
);
CREATE INDEX copy_child_orders_follow_ix ON copy_child_orders (follow_id, status);
CREATE INDEX copy_child_orders_fill_ix   ON copy_child_orders (master_trade_id);

CREATE TABLE high_water_marks (
    follow_id       BIGINT PRIMARY KEY REFERENCES copy_follows (follow_id),
    watermark_pnl   DECIMAL(28,8) NOT NULL DEFAULT 0,  -- ratchet-only baseline
    last_settled_at TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE profit_share_accruals (
    accrual_id        BIGSERIAL PRIMARY KEY,
    follow_id         BIGINT NOT NULL REFERENCES copy_follows (follow_id),
    strategy_id       BIGINT NOT NULL REFERENCES strategy_profiles (strategy_id),
    period_start      TIMESTAMPTZ NOT NULL,
    period_end        TIMESTAMPTZ NOT NULL,
    pnl               DECIMAL(28,8) NOT NULL,          -- cumulative computed P&L
    currency          VARCHAR(3)    NOT NULL,
    watermark_before  DECIMAL(28,8) NOT NULL,
    watermark_after   DECIMAL(28,8) NOT NULL,          -- ratchets iff accrued > 0
    accrued_amount    DECIMAL(28,8) NOT NULL DEFAULT 0 CHECK (accrued_amount >= 0),
    journal_entry_id  BIGINT REFERENCES journal_entries (id),
    status            profit_share_status_enum NOT NULL DEFAULT 'ACCRUED',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (follow_id, period_end)                     -- one accrual per period
);
CREATE INDEX profit_share_accruals_strategy_ix
    ON profit_share_accruals (strategy_id, period_end);

COMMIT;
