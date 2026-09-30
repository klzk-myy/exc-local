-- Migration 041: collateral_schedule (spec §5.24, Phase-19 Task 19.3.8).
--
-- Cash-collateral eligibility + haircut + concentration policy. v1 scope is
-- fiat cash balances only (non-cash collateral is explicitly deferred); a
-- currency absent from the schedule contributes zero to margin equity —
-- the CollateralValuator fails closed on unknown/ineligible entries.
--
-- haircut_pct is the PERCENTAGE DEDUCTED (§27.1 matrix convention:
-- "haircut_pct" 5.00 ⇒ the asset contributes 95% of its mark value).
-- max_concentration_pct bounds the share of account collateral equity a
-- single non-base currency may supply (§5.24: excess is valued at zero
-- for margining but remains withdrawable).
BEGIN;

CREATE TABLE IF NOT EXISTS collateral_schedule (
    id                    BIGSERIAL PRIMARY KEY,
    currency              VARCHAR(3) NOT NULL UNIQUE
        CHECK (currency ~ '^[A-Z]{3}$'),
    eligible              BOOLEAN NOT NULL DEFAULT false,
    haircut_pct           DECIMAL(5,2) NOT NULL DEFAULT 100.00
        CHECK (haircut_pct >= 0 AND haircut_pct <= 100),
    max_concentration_pct DECIMAL(5,2) NOT NULL DEFAULT 30.00
        CHECK (max_concentration_pct > 0 AND max_concentration_pct <= 100),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_collateral_schedule_eligible
    ON collateral_schedule (eligible);

-- Seed the fiat tier rows named by the task/spec (USD 0%, EUR 0.5%,
-- JPY 1%, exotic 3–10%); remaining G10 sits inside the same indicative
-- band. Idempotent so replays do not clobber admin edits.
INSERT INTO collateral_schedule (currency, eligible, haircut_pct, max_concentration_pct) VALUES
    ('USD', true, 0.00,  100.00),
    ('EUR', true, 0.50,   40.00),
    ('GBP', true, 0.50,   40.00),
    ('CHF', true, 1.00,   30.00),
    ('JPY', true, 1.00,   30.00),
    ('CAD', true, 1.00,   30.00),
    ('AUD', true, 2.00,   30.00),
    ('NZD', true, 2.00,   30.00),
    ('SGD', true, 2.00,   30.00),
    ('SEK', true, 2.00,   25.00),
    ('NOK', true, 2.00,   25.00),
    ('MXN', true, 5.00,   15.00),
    ('ZAR', true, 8.00,   10.00),
    ('TRY', true, 10.00,  10.00),
    ('BRL', true, 10.00,  10.00)
ON CONFLICT (currency) DO NOTHING;

COMMIT;
