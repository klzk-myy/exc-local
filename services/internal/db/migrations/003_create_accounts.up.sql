-- 003_create_accounts.up.sql
-- Spec §5.2 accounts — baseline columns only.
-- Excluded (owned by later migrations): client_category (042), max_sub_accounts (067),
--   product_profile_id / swapfree_status (095), umr_in_scope (043),
--   settlement_intent (104), trade_group_id (072), vip_tier (086),
--   default_stp_mode (094), base_currency / employee_account / pep_status (#35).
-- risk_limits_id / fee_tier_id are plain BIGINT here; the FK constraints are
-- added in 020_create_indexes once risk_limits (011) and fee_tiers (012) exist.

BEGIN;

CREATE TYPE account_type_enum   AS ENUM ('SPOT', 'MARGIN', 'PORTFOLIO');
CREATE TYPE kyc_tier_enum       AS ENUM ('T0', 'T1', 'T2');
CREATE TYPE account_status_enum AS ENUM ('ACTIVE', 'SUSPENDED', 'FROZEN', 'CLOSED');

CREATE TABLE accounts (
    id                BIGSERIAL PRIMARY KEY,
    user_id           BIGINT NOT NULL REFERENCES users (id),
    account_type      account_type_enum   NOT NULL,
    kyc_tier          kyc_tier_enum       NOT NULL DEFAULT 'T0',
    status            account_status_enum NOT NULL DEFAULT 'ACTIVE',
    parent_account_id BIGINT REFERENCES accounts (id),   -- NULL for master accounts
    risk_limits_id    BIGINT,                            -- FK added in migration 020
    fee_tier_id       BIGINT,                            -- FK added in migration 020
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
