-- 235_account_margin_thresholds.up.sql
-- Phase-19 Task 19.3.16 (spec §13.3, §13.6d, §24 #163).
--
-- Per-account margin-threshold overrides for the margin-level status
-- machine: warning / margin-call / stop-out percentages. Absent row =
-- defaults resolved from the account's client_category (ESMA retail
-- 120%/100%/50%; professional floor 100%/80%/30%; institutional rows
-- are explicit here).
--
-- The CHECK enforces the only universal invariant: strictly ordered,
-- positive thresholds. Regulatory floors (retail cannot weaken below
-- the ESMA intervention values) are service-layer guards in
-- risk.MarginThresholdService.Set — a floor violation is a caller
-- bug/policy decision, not a database integrity failure.

BEGIN;

CREATE TABLE account_margin_thresholds (
    account_id      BIGINT PRIMARY KEY REFERENCES accounts (id),
    warning_pct     DECIMAL(10,4) NOT NULL,
    margin_call_pct DECIMAL(10,4) NOT NULL,
    stop_out_pct    DECIMAL(10,4) NOT NULL,
    updated_by      BIGINT,                            -- caller user id
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT account_margin_thresholds_order_chk
        CHECK (warning_pct > margin_call_pct
               AND margin_call_pct > stop_out_pct
               AND stop_out_pct > 0)
);

COMMIT;
