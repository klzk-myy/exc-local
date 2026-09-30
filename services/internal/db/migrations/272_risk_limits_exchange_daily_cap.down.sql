-- 272_risk_limits_exchange_daily_cap.down.sql

BEGIN;

ALTER TABLE risk_limits
    DROP COLUMN IF EXISTS exchange_daily_withdraw_limit;

COMMIT;
