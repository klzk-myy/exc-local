-- Migration 232: pb_credit_reservations (Phase-19 Task 19.3.7).
--
-- Durable per-order PB NOP/DSL headroom debits keyed on the order id —
-- the reservation contract named by the task body:
-- (order_id, reserved_amount, expiry).
--
-- pb_credit_limits.current_net_open_position / current_daily_settled
-- carry the running counters (migration 037); this table records WHICH
-- order holds how much so cancel/reject/expiry can credit back exactly
-- what was debited — the release is therefore crash-safe and idempotent
-- (status transition ACTIVE→RELEASED is once-only). A reservation
-- debits both counters by the same USD notional: an accepted order may
-- fill (adds to NOP) and would settle today (adds to DSL) — the
-- reservation bounds the worst case.
BEGIN;

CREATE TABLE IF NOT EXISTS pb_credit_reservations (
    id                  BIGSERIAL PRIMARY KEY,
    order_id            BIGINT NOT NULL,
    client_account_id   BIGINT NOT NULL,
    limit_id            BIGINT NOT NULL REFERENCES pb_credit_limits(id),
    reserved_amount     DECIMAL(28,8) NOT NULL CHECK (reserved_amount > 0),
    status              VARCHAR(16) NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE', 'CONSUMED', 'RELEASED')),
    expires_at          TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at         TIMESTAMPTZ
);

-- At most one ACTIVE reservation row per (order, limit scope): one
-- submit may debit BOTH the global (currency_pair NULL) row and the
-- pair-scoped row, hence the composite key rather than order alone.
CREATE UNIQUE INDEX IF NOT EXISTS uq_pb_reservations_order_scope
    ON pb_credit_reservations (order_id, limit_id) WHERE status = 'ACTIVE';
CREATE INDEX IF NOT EXISTS idx_pb_reservations_account
    ON pb_credit_reservations (client_account_id, status);

COMMIT;
