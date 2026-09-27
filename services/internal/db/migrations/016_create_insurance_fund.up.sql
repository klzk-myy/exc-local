-- 016_create_insurance_fund.up.sql
-- Spec §5.15 insurance_fund — one row per currency; depletion_threshold
-- breach triggers AUTO_HALT (§13).

BEGIN;

CREATE TABLE insurance_fund (
    id                  BIGSERIAL PRIMARY KEY,
    currency            VARCHAR(3) NOT NULL,
    balance             DECIMAL(28,8) NOT NULL DEFAULT 0,
    depletion_threshold DECIMAL(28,8),                     -- breach triggers AUTO_HALT
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
