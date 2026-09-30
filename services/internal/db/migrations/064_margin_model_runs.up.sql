-- 064_margin_model_runs.up.sql
-- Phase-19 Tasks 19.3.13 + 19.3.21 (spec §13.10, §13.12, §24 #206/#344).
--
--   margin_model_runs          — the validation-run register. Every
--                                stress-suite scenario and every daily
--                                backtest execution persists one row
--                                carrying its measured metrics, breach
--                                count and the independent validator.
--   margin_model_param_changes — the change-control register. Every
--                                margin/liquidation parameter proposal
--                                (floors, decay step, leverage tiers)
--                                records owner, dual-control approver
--                                and the linked validation run the
--                                ParamChangeGate verified; a rejected
--                                gate evaluation leaves a REJECTED
--                                audit row (§13.12 evidence).
--
-- Field vocabulary beyond the task's key-fields digest
-- (run_id / kind / scenario / result_metrics / breach_count /
-- created_at / reviewed_by):
--
--   status        — PASS | FAIL outcome the ParamChangeGate keys on;
--                   derivable from result_metrics but pinned as a
--                   column so a gate lookup never parses jsonb.
--   param_change  — parameter name the run validates; NULL for
--                   scheduled library sweeps (weekly STRESS, daily
--                   BACKTEST).
--   initiated_by  — executor identity; NULL = the weekly/daily
--                   scheduler (a scheduled run has no human initiator).
--   reviewed_at   — validator sign-off timestamp. reviewed_by is the
--                   validator identity per Task 19.3.21 independence:
--                   a linked parameter change whose owner equals
--                   reviewed_by is rejected MARGIN_MODEL_UNVALIDATED.
--
-- run_status lifecycle: PASS | FAIL — recorded at insert by the
-- engine; the gate accepts PASS only, so re-validation means a NEW
-- run row (runs are append-only evidence; nothing mutates a recorded
-- outcome).
--
-- param-change status lifecycle: PENDING → VALIDATED | REJECTED →
-- APPLIED. VALIDATED means the gate's checks passed; APPLIED is
-- stamped by the executor that actually mutates the parameter.

BEGIN;

CREATE TABLE margin_model_runs (
    run_id          BIGSERIAL PRIMARY KEY,
    kind            VARCHAR(8)  NOT NULL CHECK (kind IN ('STRESS','BACKTEST')),
    scenario        VARCHAR(64) NOT NULL,
    status          VARCHAR(4)  NOT NULL DEFAULT 'PASS'
                    CHECK (status IN ('PASS','FAIL')),
    result_metrics  JSONB       NOT NULL DEFAULT '{}'::jsonb,
    breach_count    INTEGER     NOT NULL DEFAULT 0 CHECK (breach_count >= 0),
    param_change    VARCHAR(64),
    initiated_by    BIGINT,
    reviewed_by     BIGINT,
    reviewed_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Suite/day sweeps and the scheduler's "latest run" probe.
CREATE INDEX margin_model_runs_kind_idx
    ON margin_model_runs (kind, created_at DESC);
-- Change-control lookups: "latest PASS run for this parameter".
CREATE INDEX margin_model_runs_param_idx
    ON margin_model_runs (param_change, status, created_at DESC)
    WHERE param_change IS NOT NULL;

CREATE TABLE margin_model_param_changes (
    id             BIGSERIAL PRIMARY KEY,
    parameter      VARCHAR(64) NOT NULL,
    proposed_value JSONB       NOT NULL,
    owner_id       BIGINT      NOT NULL,          -- change author
    approved_by    BIGINT,                        -- dual-control second principal
    run_id         BIGINT REFERENCES margin_model_runs (run_id),
    status         VARCHAR(16) NOT NULL DEFAULT 'PENDING'
                   CHECK (status IN ('PENDING','VALIDATED','REJECTED','APPLIED')),
    reject_reason  VARCHAR(255),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at     TIMESTAMPTZ,
    -- §8.2 four-eyes: a gated change needs a distinct approver, same
    -- convention as insurance_fund_adjustments (migration 230).
    CONSTRAINT margin_param_change_dual CHECK (approved_by IS NULL OR approved_by <> owner_id)
);

CREATE INDEX margin_model_param_changes_status_idx
    ON margin_model_param_changes (status, created_at DESC);
CREATE INDEX margin_model_param_changes_param_idx
    ON margin_model_param_changes (parameter, created_at DESC);

COMMIT;
