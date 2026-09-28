-- 119_commission_engine.up.sql
-- Phase-03 Task 3.3.13 — Commission Engine & Dual Fee Model
-- (spec §8.5 + §5.41 pricing_plan, §24 #223).
--
-- Two artefacts:
--   1. commission_tiers — volume-tiered commission schedule for
--      RAW_SPREAD_COMMISSION accounts: the highest tier whose
--      min_monthly_volume is met applies. Rates are denominated in the
--      instrument's QUOTE currency; either leg may be zero (per-lot only,
--      per-million only, or a blended schedule).
--   2. account_monthly_volume — per-account calendar-month notional
--      volume tracker (USD equivalent) feeding tier resolution. The
--      commission engine upserts one row per (account, month); the
--      calendar boundary is the PRIMARY KEY — no reset job exists.
--
-- The account's fee model itself (SPREAD_MARKUP vs RAW_SPREAD_COMMISSION)
-- is NOT a column here: it is account_product_profiles.pricing_plan
-- (migration 095, Phase-14 Task 14.3.13) — the single source per §5.41.

BEGIN;

CREATE TABLE commission_tiers (
    tier_id             BIGSERIAL PRIMARY KEY,
    tier_name           VARCHAR(32)  NOT NULL UNIQUE,
    min_monthly_volume  DECIMAL(28,8) NOT NULL DEFAULT 0,  -- USD-equivalent monthly notional lower bound
    rate_per_lot        DECIMAL(20,8) NOT NULL DEFAULT 0,  -- per standard lot, quote currency
    rate_per_million    DECIMAL(20,8) NOT NULL DEFAULT 0,  -- per 1e6 quote-currency notional
    created_at          TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CHECK (min_monthly_volume >= 0),
    CHECK (rate_per_lot >= 0 AND rate_per_million >= 0)
);

-- Deterministic ordering for boundary resolution: no two tiers may share a
-- volume threshold (the engine picks the greatest qualifying bound).
CREATE UNIQUE INDEX commission_tiers_min_volume_ux
    ON commission_tiers (min_monthly_volume);

-- Default ladder — higher monthly volume earns lower commission. Rate
-- denominations are quote-currency units; the zero floor is tier 0.
INSERT INTO commission_tiers
    (tier_name, min_monthly_volume, rate_per_lot, rate_per_million)
VALUES
    ('STANDARD',      0,         7.00,  70.00),
    ('ACTIVE',        1000000,   6.00,  60.00),
    ('PROFESSIONAL',  25000000,  5.00,  50.00),
    ('INSTITUTIONAL', 100000000, 4.00,  40.00),
    ('PRIME',         500000000, 3.00,  30.00);

CREATE TABLE account_monthly_volume (
    account_id    BIGINT        NOT NULL REFERENCES accounts (id),
    month         DATE          NOT NULL,               -- UTC calendar month, day always = 1
    volume_usd    DECIMAL(28,8) NOT NULL DEFAULT 0,     -- aggregate notional, USD equivalent
    fill_count    BIGINT        NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ   NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, month),
    CHECK (volume_usd >= 0),
    CHECK (date_trunc('month', month)::date = month)
);

COMMIT;
