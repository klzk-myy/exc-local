-- 011_create_risk_limits.up.sql
-- Spec §5.10 risk_limits. account_id NULL = global default;
-- symbol NULL = all symbols ('*' = wildcard); tier uses kyc_tier_enum (created in 003).

BEGIN;

CREATE TABLE risk_limits (
    id                     BIGSERIAL PRIMARY KEY,
    account_id             BIGINT,                          -- NULL = global default
    symbol                 VARCHAR(32),                     -- NULL = all symbols; '*' = wildcard
    tier                   kyc_tier_enum,                   -- KYC tier default
    max_order_qty          DECIMAL(28,8),
    max_daily_volume       DECIMAL(28,8),
    max_open_orders        INTEGER,
    daily_withdraw_limit   DECIMAL(28,8),
    max_withdraw_amount    DECIMAL(28,8),
    withdraw_rate_per_hour DECIMAL(28,8)                    -- NULL = global default
);

COMMIT;
