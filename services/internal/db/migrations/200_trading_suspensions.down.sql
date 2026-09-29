-- 200_trading_suspensions.down.sql — drops the Task 11.3.4/11.3.8/11.3.12
-- suspension record table and its enums.

BEGIN;

DROP TABLE IF EXISTS trading_suspensions;
DROP TYPE IF EXISTS suspension_state_enum;
DROP TYPE IF EXISTS suspension_scope_enum;

COMMIT;
