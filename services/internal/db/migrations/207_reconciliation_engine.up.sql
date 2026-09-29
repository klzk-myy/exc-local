-- 207_reconciliation_engine.up.sql
-- Phase-13 Task 13.3.2 — Reconciliation Engine durable report surface
-- (spec §2.7 fail-closed pessimism; Phase-13 Task 13.3.2 DoD
-- "reconciliation report generated").
--
-- Two tables:
--   1. reconciliation_runs — one row per engine sweep (hourly cadence).
--      status vocabulary:
--        RUNNING      — sweep in progress (crash-safe: a run stuck
--                       RUNNING is itself an operability signal)
--        CLEAN        — every category produced findings==0
--        MISMATCH     — >=1 MISMATCH finding (P1 + scoped auto-halt)
--        INCONCLUSIVE — no MISMATCH but >=1 leg could not be verified
--                       (fail-closed: unverifiable input is never a pass)
--        ERROR        — persistence-level failure during the run
--   2. reconciliation_findings — one row per divergent (category, subject)
--      pair, carrying the fixed-point (8dp) expected/actual/delta triple
--      the zero-budget policy requires.
--
-- severity vocabulary (not an enum so future INFO-level rows slot in):
--   MISMATCH      — verified divergence; P1 alert + auto-halt per the
--                   category→scope map in internal/reconciliation.
--   INCONCLUSIVE  — a required input leg was unreachable/absent
--                   (core snapshot seam, bank statements, WAL window);
--                   P1 alert, no halt — ops investigates.

BEGIN;

CREATE TABLE reconciliation_runs (
    id                 BIGSERIAL PRIMARY KEY,
    started_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at        TIMESTAMPTZ,
    status             VARCHAR(12) NOT NULL DEFAULT 'RUNNING'
                       CHECK (status IN ('RUNNING','CLEAN','MISMATCH','INCONCLUSIVE','ERROR')),
    categories_checked INT         NOT NULL DEFAULT 0,
    findings_count     INT         NOT NULL DEFAULT 0,
    mismatch_count     INT         NOT NULL DEFAULT 0,
    inconclusive_count INT         NOT NULL DEFAULT 0,
    halts_emitted      JSONB,                    -- [{scope,target,reason,suspension_id}]
    error              TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX reconciliation_runs_status_ix
    ON reconciliation_runs (status, started_at DESC);
CREATE INDEX reconciliation_runs_started_ix
    ON reconciliation_runs (started_at DESC);

CREATE TABLE reconciliation_findings (
    id          BIGSERIAL PRIMARY KEY,
    run_id      BIGINT       NOT NULL REFERENCES reconciliation_runs (id) ON DELETE CASCADE,
    category    VARCHAR(24)  NOT NULL,           -- BALANCES|POSITIONS|ORDERS|TRADES|FUNDING|SETTLEMENT|FEES|PNL|GENERAL_LEDGER
    subject     VARCHAR(160) NOT NULL DEFAULT '', -- e.g. account:42:USD | order:1004 | currency:USD
    leg         VARCHAR(64)  NOT NULL DEFAULT '', -- which comparison leg produced it (e.g. wal_replay, ledger_diff, statement)
    expected    DECIMAL(28,8),                   -- NULL = leg not measurable
    actual      DECIMAL(28,8),
    delta       DECIMAL(28,8),
    unit        VARCHAR(16)  NOT NULL DEFAULT 'AMOUNT'
                CHECK (unit IN ('AMOUNT','COUNT','QTY','STATE')),
    severity    VARCHAR(12)  NOT NULL
                CHECK (severity IN ('MISMATCH','INCONCLUSIVE','INFO')),
    halt_scope  VARCHAR(24),                     -- kill-switch scope when a halt was emitted for this finding
    halt_target VARCHAR(128),
    detail      JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX reconciliation_findings_run_ix
    ON reconciliation_findings (run_id, severity);
CREATE INDEX reconciliation_findings_category_ix
    ON reconciliation_findings (category, created_at DESC);

COMMIT;
