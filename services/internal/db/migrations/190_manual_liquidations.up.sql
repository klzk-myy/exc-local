-- 190_manual_liquidations.up.sql
-- Phase-05 Task 5.3.30 — durable record of dual-control manual
-- liquidation requests (POST /api/v1/admin/liquidation/manual).
--
-- One row per accepted request: the account lock, position snapshot,
-- this insert, the admin_audit_log row and the audit_hash_chain link all
-- commit atomically in the handler's transaction — the audit trail
-- cannot exist without the liquidation record or vice versa.
--
-- status lifecycle: DISPATCHED (recorded + WAL event emitted) →
-- COMPLETED | FAILED once the Phase-19 auction machinery reports;
-- EMIT_FAILED when the post-commit event publish failed (operators
-- re-issue under a fresh dual-control approval).

BEGIN;

CREATE TABLE manual_liquidations (
    id               BIGSERIAL    PRIMARY KEY,
    account_id       BIGINT       NOT NULL REFERENCES accounts (id),
    instrument_id    BIGINT       REFERENCES instruments (id),   -- NULL = all open positions
    reason           TEXT         NOT NULL,
    override_auction BOOLEAN      NOT NULL DEFAULT FALSE,        -- skips the 5s CALL phase
    source           VARCHAR(8)   NOT NULL DEFAULT 'MANUAL',
    status           VARCHAR(16)  NOT NULL DEFAULT 'DISPATCHED'
                     CHECK (status IN ('DISPATCHED','COMPLETED','FAILED','EMIT_FAILED')),
    initiated_by     BIGINT       NOT NULL,                      -- Risk Manager / Super Admin
    approved_by      BIGINT       NOT NULL,                      -- distinct second approver (four-eyes)
    positions        JSONB        NOT NULL DEFAULT '[]'::jsonb,  -- estimated fills snapshot
    audit_seq        BIGINT,                                     -- audit_hash_chain.sequence_num
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT manual_liq_dual_control CHECK (initiated_by <> approved_by)
);

CREATE INDEX idx_manual_liquidations_account
    ON manual_liquidations (account_id, created_at DESC);
CREATE INDEX idx_manual_liquidations_status
    ON manual_liquidations (status) WHERE status IN ('DISPATCHED','EMIT_FAILED');

COMMIT;
