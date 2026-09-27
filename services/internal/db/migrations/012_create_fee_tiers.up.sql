-- 012_create_fee_tiers.up.sql
-- Spec §5.11 fee_tiers.

BEGIN;

CREATE TABLE fee_tiers (
    id              BIGSERIAL PRIMARY KEY,
    tier_name       VARCHAR(32) NOT NULL,                   -- e.g. institutional, professional, standard, basic
    maker_bps       DECIMAL(6,4) NOT NULL,
    taker_bps       DECIMAL(6,4) NOT NULL,
    promo_until     TIMESTAMPTZ,                            -- NULL = no promo
    promo_maker_bps DECIMAL(6,4),
    promo_taker_bps DECIMAL(6,4)
);

COMMIT;
