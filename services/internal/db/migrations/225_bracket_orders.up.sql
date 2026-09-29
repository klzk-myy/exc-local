-- 225_bracket_orders.up.sql
-- Phase-16 Task 16.3.14 — bracket / OTO composite orders (spec §6.2).
--
-- A bracket binds a parent entry order to a stop-loss + take-profit
-- child configuration. While the parent is open the group state is
-- WORKING; each parent fill atomically places a proportional SL/TP
-- pair through the Phase-14 OCO machinery (orders.oco_group_id +
-- oco_group_link, migration 218) and appends a bracket_children row.
-- `placed_qty` accumulates the child quantity already covered, so a
-- restart can re-derive the uncovered remainder
-- (parent.filled_qty − placed_qty) and re-drive placement
-- deterministically — the group row plus the children ledger are the
-- full WAL-recoverable linkage.

BEGIN;

CREATE TYPE bracket_state_enum AS ENUM (
    'WORKING',    -- parent live; fills spawn proportional children
    'FILLED',     -- parent fully filled, all children placed
    'CANCELLED',  -- parent cancelled; placed children cancelled too
    'FAILED'      -- parent rejected or child placement failed
);

CREATE TABLE bracket_orders (
    id               BIGSERIAL PRIMARY KEY,
    account_id       BIGINT NOT NULL REFERENCES accounts (id),
    instrument_id    BIGINT NOT NULL REFERENCES instruments (id),
    parent_order_id  BIGINT NOT NULL UNIQUE REFERENCES orders (id),
    parent_side      order_side_enum NOT NULL,
    child_sl         JSONB NOT NULL,        -- {trigger_price, limit_price?}
    child_tp         JSONB NOT NULL,        -- {trigger_price, limit_price?}
    state            bracket_state_enum NOT NULL DEFAULT 'WORKING',
    placed_qty       DECIMAL(28,8) NOT NULL DEFAULT 0,
    child_seq        INTEGER NOT NULL DEFAULT 0,
    gtd_expiry       TIMESTAMPTZ,           -- children inherit parent GTD
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX bracket_orders_open_idx
    ON bracket_orders (state) WHERE state IN ('WORKING');

CREATE TABLE bracket_children (
    id           BIGSERIAL PRIMARY KEY,
    bracket_id   BIGINT NOT NULL REFERENCES bracket_orders (id) ON DELETE CASCADE,
    pair_index   SMALLINT NOT NULL,
    qty          DECIMAL(28,8) NOT NULL,
    sl_order_id  BIGINT NOT NULL,
    tp_order_id  BIGINT NOT NULL,
    oco_group_id BIGINT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (bracket_id, pair_index)
);

CREATE INDEX bracket_children_orders_idx ON bracket_children (sl_order_id, tp_order_id);

COMMIT;
