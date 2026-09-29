-- 200_trading_suspensions.up.sql
-- Phase-11 Tasks 11.3.4 / 11.3.8 / 11.3.12 — durable record of every
-- trading-suspension (kill-switch) activation and clearance.
--
-- Redis `halt:*` keys are the hot-path enforcement flags (checked per
-- order admission); this table is the durable control-plane record —
-- PostgreSQL is authoritative so the ACTIVE set can rebuild Redis after
-- a cache flush or failover (admin.KillSwitchService.ReconcileFlags).
-- The admin_audit_log row for each transition commits in the same
-- transaction via admin.Log; this table carries the suspension payload
-- itself.
--
-- Scope lattice: GLOBAL + §7.2/§24 #152 scopes (ACCOUNT, INSTRUMENT,
-- FIX_SESSION) + Task 11.3.12/§24 #409 scopes (COUNTERPARTY, LP, RAIL)
-- + venue-administration axes (INSTRUMENT_CLASS, REGION, ENV, DESK).
-- Resolution precedence on the order path is first-match-wins in the
-- resolver's ordering: ACCOUNT → COUNTERPARTY → FIX_SESSION →
-- INSTRUMENT → INSTRUMENT_CLASS → DESK → REGION → ENV → GLOBAL.

BEGIN;

CREATE TYPE suspension_scope_enum AS ENUM (
    'GLOBAL', 'ACCOUNT', 'COUNTERPARTY', 'INSTRUMENT', 'INSTRUMENT_CLASS',
    'FIX_SESSION', 'LP', 'RAIL', 'REGION', 'ENV', 'DESK'
);
CREATE TYPE suspension_state_enum AS ENUM ('ACTIVE', 'CLEARED');

CREATE TABLE trading_suspensions (
    suspension_id     BIGSERIAL PRIMARY KEY,
    scope             suspension_scope_enum NOT NULL,
    target_id         VARCHAR(128) NOT NULL DEFAULT '',     -- '' for GLOBAL
    reason            TEXT NOT NULL,
    state             suspension_state_enum NOT NULL DEFAULT 'ACTIVE',
    initiated_by      BIGINT NOT NULL,                      -- admin user_id
    approved_by       BIGINT,                               -- dual-control approver (required for GLOBAL)
    cleared_by        BIGINT,
    clear_approved_by BIGINT,                               -- dual-control approver on reset
    cleared_reason    TEXT,
    client_ip         VARCHAR(64),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    cleared_at        TIMESTAMPTZ
);

-- One ACTIVE suspension per (scope, target) — a second SET on an already
-- halted target is a conflict, not a duplicate row.
CREATE UNIQUE INDEX trading_suspensions_active_uq
    ON trading_suspensions (scope, target_id) WHERE state = 'ACTIVE';
CREATE INDEX idx_trading_suspensions_state
    ON trading_suspensions (state, created_at DESC);

COMMIT;
