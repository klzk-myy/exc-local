-- 011_create_risk_limits.down.sql
BEGIN;

DROP TABLE IF EXISTS risk_limits CASCADE;
-- kyc_tier_enum is owned by 003_create_accounts — not dropped here.

COMMIT;
