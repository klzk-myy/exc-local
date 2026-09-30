-- 249_basel_reports.up.sql (renumbered from 240 during the wave-2
-- numbering merge — 241..248 were claimed by concurrent Phase-21 tasks;
-- append-only ordering keeps this at the tail)
-- Phase-21 Task 21.3.13 — Basel III capital adequacy & leverage ratio
-- reporting (spec §14.1; §24 #151; §27.1 Basel III matrix →
-- CAPITAL_ADEQUACY_BREACH / LEVERAGE_RATIO_BREACH). Number is the next
-- free slot — the task prescribes basel.go only, and stored report
-- versions (AC "report versions retained + exportable") need a home;
-- no plan-reserved number was cited.
--
--   basel_reports — append-only daily-EOD + on-demand report versions.
--                   snapshot_key is the idempotent dedup anchor
--                   ('eod:{period}' for the sweep, 'regen:{token}' for
--                   officer-triggered regeneration) — the daily job is
--                   re-run safe. inputs carries the GL account-code →
--                   balance reconciliation map the report derives from,
--                   and inputs_complete=false is the fail-closed flag
--                   when a component could not be priced into the
--                   reporting currency (the report never fabricates a
--                   total from partial inputs silently).
--
-- Retention (§19.12): prudential reports — no purge below the 7-year
-- compliance horizon.

BEGIN;

CREATE TABLE basel_reports (
    id                 BIGSERIAL    PRIMARY KEY,
    period             DATE         NOT NULL,            -- EOD snapshot date (UTC)
    reporting_currency CHAR(3)      NOT NULL DEFAULT 'USD',
    tier1_capital      NUMERIC(28,8) NOT NULL,
    tier2_capital      NUMERIC(28,8) NOT NULL DEFAULT 0,
    total_capital      NUMERIC(28,8) NOT NULL,
    rwa                NUMERIC(28,8) NOT NULL,           -- risk-weighted assets
    leverage_exposure  NUMERIC(28,8) NOT NULL,
    car                NUMERIC(14,6) NOT NULL,           -- total_capital / rwa
    leverage_ratio     NUMERIC(14,6) NOT NULL,           -- tier1 / leverage_exposure
    car_breach         BOOLEAN      NOT NULL DEFAULT FALSE, -- car < 8%
    leverage_breach    BOOLEAN      NOT NULL DEFAULT FALSE, -- leverage_ratio < 3%
    inputs_complete    BOOLEAN      NOT NULL DEFAULT TRUE,  -- fail-closed flag (SDD edge case)
    inputs             JSONB        NOT NULL DEFAULT '{}',  -- GL code→balance reconciliation + exposure components
    snapshot_key       VARCHAR(160) NOT NULL,            -- idempotent dedup anchor
    generated_by       VARCHAR(64)  NOT NULL DEFAULT 'eod-sweep',
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (snapshot_key)
);

-- Officer surface: newest snapshot per period, breach triage queue.
CREATE INDEX ix_basel_reports_period  ON basel_reports (period DESC, id DESC);
CREATE INDEX ix_basel_reports_breaches
    ON basel_reports (created_at DESC)
    WHERE car_breach OR leverage_breach OR NOT inputs_complete;

COMMIT;
