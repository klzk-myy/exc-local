-- 071_grid_bots.down.sql — Phase-16 Task 16.3.19 rollback.

BEGIN;

DROP TABLE IF EXISTS grid_bot_orders;
DROP TABLE IF EXISTS grid_bots;
DROP TYPE IF EXISTS grid_child_status_enum;
DROP TYPE IF EXISTS grid_bot_status_enum;
DROP TYPE IF EXISTS grid_mode_enum;

COMMIT;
