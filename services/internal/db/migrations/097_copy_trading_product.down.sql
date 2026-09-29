-- 097_copy_trading_product.down.sql — reverse of 097 (Task 14.3.14).

BEGIN;

DROP TABLE IF EXISTS profit_share_accruals;
DROP TABLE IF EXISTS high_water_marks;
DROP TABLE IF EXISTS copy_child_orders;
DROP TABLE IF EXISTS copy_follows;
DROP TABLE IF EXISTS strategy_profiles;

DROP TYPE IF EXISTS profit_share_status_enum;
DROP TYPE IF EXISTS copy_child_status_enum;
DROP TYPE IF EXISTS copy_follow_status_enum;
DROP TYPE IF EXISTS copy_safety_mode_enum;
DROP TYPE IF EXISTS copy_strategy_status_enum;

COMMIT;
