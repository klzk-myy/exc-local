-- 224_algo_orders.up.sql
-- Phase-16 Tasks 16.3.1/16.3.2/16.3.6/16.3.7/16.3.8/16.3.12/16.3.18 —
-- durable parent state for the algo order framework plus the child-order
-- linkage/audit trail (spec §6.2 advanced order types, §6.10 visibility,
-- §24 #48/#49/#52/#53/#275).
--
-- algo_orders is the PARENT record. TWAP/VWAP/VP are strategy names over
-- base order types (no new orders.order_type enum values — the §5.4
-- enum's TWAP/VWAP/SPREAD/SCALE values describe plan-level intents; the
-- executed children are ordinary LIMIT orders dispatched through the
-- Phase-05 order pipeline). Migration 038 (Task 16.3.10, sibling-owned)
-- lands orders.algo_type/algo_params for engine-carried params; until it
-- merges, strategy parameters live in algo_orders.params JSONB — when 038
-- lands the children can carry a copy for WAL replay without a backfill
-- here (documented seam, not a schema gap).
--
-- Status state machine (Task 16.3.8):
--   NEW → PENDING → RUNNING → PAUSED → RUNNING → COMPLETED
--                                        ↘→ CANCELLED / EXPIRED / FAILED
--   NEW     — row inserted, activation pending (in-request transient).
--   PENDING — scheduled: start_at in the future (delayed dispatch).
--   RUNNING — driver goroutine owns the parent, dispatching children.
--   PAUSED  — driver quiesced; open child slices are cancelled on pause.
--   COMPLETED — all slices dispatched and children terminal / duration end.
--   CANCELLED — operator/client cancel; open children were terminated.
--   EXPIRED — max_duration / expires_at reached before total_qty filled.
--   FAILED  — synchronous rejection recorded for audit (spread legs that
--             failed the market-spread check or unwind-compensated) — the
--             plan-doc enum has no failure terminal; FAILED carries it so
--             a rejected spread never masquerades as a client cancel.

BEGIN;

CREATE TABLE algo_orders (
    id              BIGSERIAL PRIMARY KEY,
    account_id      BIGINT       NOT NULL REFERENCES accounts (id),
    algo_type       VARCHAR(8)   NOT NULL
                    CHECK (algo_type IN ('TWAP','VWAP','VP','SCALE','SPREAD')),
    symbol          VARCHAR(32)  NOT NULL,   -- primary symbol (SPREAD: leg1)
    side            VARCHAR(4)   NOT NULL CHECK (side IN ('BUY','SELL')),
    status          VARCHAR(9)   NOT NULL DEFAULT 'NEW'
                    CHECK (status IN ('NEW','PENDING','RUNNING','PAUSED',
                                      'COMPLETED','CANCELLED','EXPIRED','FAILED')),
    total_qty       DECIMAL(28,8) NOT NULL CHECK (total_qty > 0),
    filled_qty      DECIMAL(28,8) NOT NULL DEFAULT 0,
    params          JSONB        NOT NULL DEFAULT '{}'::jsonb,
    state           JSONB        NOT NULL DEFAULT '{}'::jsonb, -- driver cursor + audit trail
    start_at        TIMESTAMPTZ,             -- NULL = immediate; future = delayed dispatch
    expires_at      TIMESTAMPTZ,             -- optional hard deadline
    client_order_id VARCHAR(64),             -- optional idempotent parent submit key
    error_detail    TEXT         NOT NULL DEFAULT '',          -- terminal reason (FAILED/CANCELLED/EXPIRED)
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ
);

-- Idempotent parent submission: same (account, client_order_id) replays
-- the stored parent rather than double-scheduling (spec §8.7 semantics).
CREATE UNIQUE INDEX algo_orders_client_id_uidx
    ON algo_orders (account_id, client_order_id)
    WHERE client_order_id IS NOT NULL;

CREATE INDEX algo_orders_account_idx
    ON algo_orders (account_id, status, created_at DESC);

-- Delayed-dispatch sweeper: PENDING parents awaiting their start_at.
CREATE INDEX algo_orders_pending_start_idx
    ON algo_orders (start_at) WHERE status = 'PENDING';

-- Restart recovery: non-terminal parents the engine must re-adopt.
CREATE INDEX algo_orders_active_idx
    ON algo_orders (id) WHERE status IN ('PENDING','RUNNING','PAUSED');

CREATE TABLE algo_order_children (
    id              BIGSERIAL PRIMARY KEY,
    algo_order_id   BIGINT       NOT NULL REFERENCES algo_orders (id),
    seq             INTEGER      NOT NULL,   -- monotonic per parent
    slice_index     INTEGER      NOT NULL,   -- schedule position (legs: 0/1)
    role            VARCHAR(8)   NOT NULL DEFAULT 'SLICE'
                    CHECK (role IN ('SLICE','LEG','UNWIND')),
    symbol          VARCHAR(32)  NOT NULL,
    side            VARCHAR(4)   NOT NULL CHECK (side IN ('BUY','SELL')),
    client_order_id VARCHAR(64)  NOT NULL,   -- "algo:{parent}:{seq}" — idempotent dispatch
    order_id        BIGINT       REFERENCES orders (id),
    qty             DECIMAL(28,8) NOT NULL,
    price           DECIMAL(20,8),
    status          VARCHAR(12)  NOT NULL DEFAULT 'PENDING'
                    CHECK (status IN ('PENDING','SUBMITTED','OPEN','FILLED',
                                      'PARTIAL','CANCELLED','REJECTED')),
    filled_qty      DECIMAL(28,8) NOT NULL DEFAULT 0,
    detail          TEXT         NOT NULL DEFAULT '', -- reject reason / notes
    dispatched_at   TIMESTAMPTZ,
    resolved_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (algo_order_id, seq),
    UNIQUE (algo_order_id, client_order_id)
);

CREATE INDEX algo_children_order_idx
    ON algo_order_children (order_id) WHERE order_id IS NOT NULL;

COMMIT;
