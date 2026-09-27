-- 013_create_margin_accounts.up.sql
-- Spec §5.12 margin_accounts.

BEGIN;

CREATE TYPE margin_mode_enum           AS ENUM ('ISOLATED', 'CROSS', 'PORTFOLIO');
CREATE TYPE margin_account_status_enum AS ENUM ('NORMAL', 'MARGIN_CALL', 'LIQUIDATING');

CREATE TABLE margin_accounts (
    id                 BIGSERIAL PRIMARY KEY,
    account_id         BIGINT NOT NULL,
    margin_mode        margin_mode_enum NOT NULL,
    equity             DECIMAL(28,8) NOT NULL DEFAULT 0,
    used_margin        DECIMAL(28,8) NOT NULL DEFAULT 0,
    available_margin   DECIMAL(28,8) NOT NULL DEFAULT 0,
    margin_utilization DECIMAL(8,6),                       -- used / equity
    status             margin_account_status_enum NOT NULL DEFAULT 'NORMAL',
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
