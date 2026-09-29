-- 075_opo_order_lists.up.sql
-- Phase-16 Task 16.3.20 — OPO / OPOCO net-proceeds order lists
-- (spec §5.39 migration-075 row: `order_lists`, `order_list_legs`,
-- `locked_proceeds`, `net_pending_qty`, `contingency_type`).
--
-- An order list binds one WORKING entry leg to one (OPO) or two (OPOCO)
-- PENDING legs. The working BUY order's net received base quantity —
-- after commission and lot rounding — becomes the pending SELL
-- quantity; the received quantity is "locked" as proceeds on the list
-- row until the pending placement lands or the list is cancelled
-- (residue below the lot boundary is unlocked on placement).
--
-- Recovery contract (§24 #287): the composite state is fully
-- reconstructable from orders + fills — `order_lists.working_order_id`
-- + `order_list_legs.order_id` carry every engine-visible link, and the
-- legs' `params` JSONB snapshots carry the pending-leg intents so a
-- restart can re-drive placement deterministically.

BEGIN;

CREATE TYPE contingency_type_enum AS ENUM ('OPO', 'OPOCO');

CREATE TYPE order_list_state_enum AS ENUM (
    'EXECUTING',   -- working leg in flight (or partially filled)
    'ALL_DONE',    -- working filled; pending leg(s) placed
    'CANCELLED',   -- client/system cancel; proceeds unlocked
    'FAILED',      -- pending-leg validation/placement failed
    'EXPIRED'      -- working leg expired before completing
);

CREATE TABLE order_lists (
    id               BIGSERIAL PRIMARY KEY,
    account_id       BIGINT NOT NULL REFERENCES accounts (id),
    instrument_id    BIGINT NOT NULL REFERENCES instruments (id),
    contingency_type contingency_type_enum NOT NULL,
    state            order_list_state_enum NOT NULL DEFAULT 'EXECUTING',
    working_order_id BIGINT NOT NULL UNIQUE REFERENCES orders (id),
    locked_proceeds  DECIMAL(28,8) NOT NULL DEFAULT 0,
    net_pending_qty  DECIMAL(28,8) NOT NULL DEFAULT 0,
    client_order_id  VARCHAR(64),
    fail_reason      VARCHAR(64),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at        TIMESTAMPTZ
);

-- List-level client dedup (idempotent resubmission, §8.7).
CREATE UNIQUE INDEX order_lists_client_uidx
    ON order_lists (account_id, client_order_id)
    WHERE client_order_id IS NOT NULL;

CREATE INDEX order_lists_account_state_idx
    ON order_lists (account_id, state, created_at DESC);

CREATE TABLE order_list_legs (
    id         BIGSERIAL PRIMARY KEY,
    list_id    BIGINT NOT NULL REFERENCES order_lists (id) ON DELETE CASCADE,
    leg_index  SMALLINT NOT NULL,
    role       VARCHAR(8) NOT NULL CHECK (role IN ('WORKING','PENDING')),
    order_id   BIGINT,                       -- NULL until placed
    params     JSONB NOT NULL,               -- SubmitRequest snapshot
    state      VARCHAR(12) NOT NULL DEFAULT 'PENDING'
               CHECK (state IN ('PENDING','PLACED','FILLED','CANCELLED','FAILED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (list_id, leg_index)
);

-- Recovery scan: find pending legs bound to an order id (cancel hooks)
-- and lists whose working order id resolves here.
CREATE INDEX order_list_legs_order_idx
    ON order_list_legs (order_id) WHERE order_id IS NOT NULL;

COMMIT;
