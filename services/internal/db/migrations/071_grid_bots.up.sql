-- 071_grid_bots.up.sql
-- Phase-16 Task 16.3.19 — Grid trading bot engine (spec §5.39 remediation-#14
-- cluster, §24 #275, §11 domain row 3, ruling R13).
--
-- Tables:
--   grid_bots        parent strategy record: price bounds, grid_count
--                    (5–200), mode (ARITHMETIC|GEOMETRIC), total_investment,
--                    leverage, TP/SL exits, cumulative realized grid PnL.
--   grid_bot_orders  per-level child accounting: one row per placed (or
--                    intentionally skipped) child order, linked to the real
--                    orders row once admitted. source_child_id makes the
--                    fill→counter-order flip idempotent under at-least-once
--                    fill delivery — at most one counter order is ever
--                    recorded per filled child.
--
-- Status lattice: RUNNING → COMPLETED (take-profit excursion) |
--                 STOPPED (user stop / stop-loss excursion) | FAILED.
-- Child lattice: PENDING → WORKING → FILLED | CANCELLED | REJECTED;
--                SKIPPED marks a deliberately unplaced leg (collision).

BEGIN;

CREATE TYPE grid_mode_enum AS ENUM ('ARITHMETIC', 'GEOMETRIC');
CREATE TYPE grid_bot_status_enum AS ENUM ('RUNNING', 'COMPLETED', 'STOPPED', 'FAILED');
CREATE TYPE grid_child_status_enum AS ENUM (
    'PENDING',     -- row claimed, dispatch to orders.Service in flight
    'WORKING',     -- child order accepted by the order pipeline
    'FILLED',      -- fully filled (counter-order placed where applicable)
    'CANCELLED',   -- cancelled by bot stop / lifecycle
    'REJECTED',    -- order pipeline rejected the child
    'SKIPPED'      -- intentionally not placed (price collision / boundary)
);

CREATE TABLE grid_bots (
    bot_id            BIGSERIAL PRIMARY KEY,
    account_id        BIGINT NOT NULL REFERENCES accounts (id),
    instrument_id     BIGINT NOT NULL REFERENCES instruments (id),
    symbol            VARCHAR(20) NOT NULL,
    lower_price       DECIMAL(20,8) NOT NULL CHECK (lower_price > 0),
    upper_price       DECIMAL(20,8) NOT NULL CHECK (upper_price > lower_price),
    grid_count        INTEGER NOT NULL CHECK (grid_count BETWEEN 5 AND 200),
    mode              grid_mode_enum NOT NULL,
    total_investment  DECIMAL(28,8) NOT NULL CHECK (total_investment > 0),
    leverage          DECIMAL(9,4) NOT NULL DEFAULT 1 CHECK (leverage >= 1),
    take_profit_price DECIMAL(20,8)
        CHECK (take_profit_price IS NULL OR take_profit_price > lower_price),
    stop_loss_price   DECIMAL(20,8)
        CHECK (stop_loss_price IS NULL OR stop_loss_price < upper_price),
    status            grid_bot_status_enum NOT NULL DEFAULT 'RUNNING',
    realized_pnl      DECIMAL(28,8) NOT NULL DEFAULT 0,
    pnl_currency      VARCHAR(3) NOT NULL,           -- instrument quote ccy
    fills_count       INTEGER NOT NULL DEFAULT 0,
    reference_price   DECIMAL(20,8),                 -- split ref at placement
    stopped_at        TIMESTAMPTZ,
    stop_reason       VARCHAR(64),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- R13: at most five concurrent RUNNING bots per account — the count check
-- runs inside the create transaction under an account row lock; this index
-- keeps the concurrent set cheap to count and list.
CREATE INDEX grid_bots_running_ix ON grid_bots (account_id) WHERE status = 'RUNNING';
CREATE INDEX grid_bots_account_ix ON grid_bots (account_id, created_at DESC);
CREATE INDEX grid_bots_instrument_ix ON grid_bots (instrument_id) WHERE status = 'RUNNING';

CREATE TABLE grid_bot_orders (
    id               BIGSERIAL PRIMARY KEY,
    bot_id           BIGINT NOT NULL REFERENCES grid_bots (bot_id),
    level_index      INTEGER NOT NULL CHECK (level_index >= 0),
    side             VARCHAR(4) NOT NULL CHECK (side IN ('BUY', 'SELL')),
    price            DECIMAL(20,8) NOT NULL CHECK (price > 0),
    qty              DECIMAL(28,8) NOT NULL CHECK (qty > 0),
    order_id         BIGINT REFERENCES orders (id),   -- set once admitted
    client_order_id  VARCHAR(64),
    status           grid_child_status_enum NOT NULL DEFAULT 'PENDING',
    filled_qty       DECIMAL(28,8) NOT NULL DEFAULT 0,
    avg_fill_price   DECIMAL(20,8),
    -- source_child_id: the filled child this counter-order was spawned by;
    -- the partial unique index makes the flip idempotent on redelivery.
    source_child_id  BIGINT REFERENCES grid_bot_orders (id),
    -- realized_pnl carries the round-trip P&L credited to grid_bots when
    -- this counter order fills (paired against source_child_id's fill).
    realized_pnl     DECIMAL(28,8) NOT NULL DEFAULT 0,
    note             VARCHAR(255),
    filled_at        TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- At most one counter order per filled source child (flip idempotency).
CREATE UNIQUE INDEX grid_bot_orders_source_ux
    ON grid_bot_orders (source_child_id) WHERE source_child_id IS NOT NULL;
-- Fill-hook lookup: engine TradeFill carries order_id.
CREATE INDEX grid_bot_orders_order_ix ON grid_bot_orders (order_id) WHERE order_id IS NOT NULL;
CREATE INDEX grid_bot_orders_bot_ix ON grid_bot_orders (bot_id, level_index);
CREATE INDEX grid_bot_orders_open_ix ON grid_bot_orders (bot_id)
    WHERE status IN ('PENDING', 'WORKING');

COMMIT;
