-- 274_capacity_reports
-- Phase-09 Task 9.3.19 — quarterly automated capacity report artifact
-- store (spec §19.9, §24 #191; docs/ops/capacity-planning.md §5). One
-- row per generator run: the full JSON report plus queryable rollups
-- (worst action, per-action counts) for ops dashboards and the DORA
-- capacity-testing evidence trail.

BEGIN;

CREATE TABLE capacity_reports (
    id              BIGSERIAL PRIMARY KEY,
    report_id       TEXT        NOT NULL UNIQUE,        -- generator run UUID
    quarter         VARCHAR(8)  NOT NULL,               -- e.g. '2026Q3'
    period_start    TIMESTAMPTZ NOT NULL,               -- regression window start
    period_end      TIMESTAMPTZ NOT NULL,               -- window end / generation time
    generated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    horizon_days    INT         NOT NULL,               -- exhaustion projection horizon (180)
    worst_action    VARCHAR(16) NOT NULL,               -- OK|PLAN_PROVISION|PROVISION|ORDER_HARDWARE|EXHAUSTED|NO_DATA
    resources_count INT         NOT NULL DEFAULT 0,
    actions         JSONB       NOT NULL DEFAULT '{}'::jsonb, -- action → count
    report          JSONB       NOT NULL                -- full ResourceReport[] payload
);
CREATE INDEX capacity_reports_quarter_ix ON capacity_reports (quarter, generated_at DESC);
CREATE INDEX capacity_reports_worst_ix   ON capacity_reports (worst_action)
    WHERE worst_action <> 'OK';

COMMIT;
