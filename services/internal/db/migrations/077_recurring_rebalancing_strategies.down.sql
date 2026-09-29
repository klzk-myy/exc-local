-- 077_recurring_rebalancing_strategies.down.sql — Phase-16 Task 16.3.21 rollback.

BEGIN;

ALTER TABLE strategies DROP CONSTRAINT IF EXISTS strategies_template_fk;
DROP TABLE IF EXISTS strategy_runs;
DROP TABLE IF EXISTS strategy_templates;
DROP TABLE IF EXISTS strategies;
DROP TYPE IF EXISTS strategy_run_status_enum;
DROP TYPE IF EXISTS strategy_template_status_enum;
DROP TYPE IF EXISTS strategy_status_enum;
DROP TYPE IF EXISTS strategy_kind_enum;

COMMIT;
