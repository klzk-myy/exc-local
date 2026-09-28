-- 109_risk_limits_exposure.down.sql

BEGIN;

DROP TABLE IF EXISTS risk_daily_usage;

ALTER TABLE risk_limits
    DROP COLUMN IF EXISTS max_notional_exposure,
    DROP COLUMN IF EXISTS max_short_exposure,
    DROP COLUMN IF EXISTS max_account_notional;

COMMIT;
