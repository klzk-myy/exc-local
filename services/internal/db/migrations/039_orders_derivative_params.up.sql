-- Migration 039 — orders: derivative order parameters (spec §5.4)
--
-- Phase-22 Task 22.3.9. Adds the per-instrument-class derivative fields
-- a submitted order may carry. Column names/types follow the canonical
-- spec §5.4 §5.4.x derivative vocabulary exactly (option_type,
-- exercise_style, barrier_type, strike, expiry_at, barrier_level,
-- value_date, near/far leg value dates, premium, premium_currency,
-- ndf_fixing_source) — plus CHECK backstops mirroring the §5.4 enums so
-- a bad write cannot land even if a caller skips application-level
-- validation (spec §2.7 fail-closed at the boundary).
--
-- Ownership notes:
--   * orders.fixing_benchmark already belongs to migration 038
--     (order_execution_params). 039 deliberately does NOT re-add it and
--     the down migration does NOT drop it. NDF fixing-source detail
--     rides the new orders.ndf_fixing_source column below — the
--     spec §5.4 note explicitly keeps it separate from the three-value
--     fixing_benchmark enum so the full §15.7 central-bank/vendor/
--     prior-day fallback hierarchy can be represented.
--   * spec §5.4 declares premium_currency VARCHAR(3) DEFAULT 'QUOTE'
--     but its own documented enum includes 'SETTLEMENT' (10 chars > 3).
--     We store VARCHAR(16) so both spec values fit; a spec-text edit is
--     out of scope for this migration.

ALTER TABLE orders
    ADD COLUMN strike               DECIMAL(20,8)  NULL, -- OPTION strike (quote per base)
    ADD COLUMN option_type          VARCHAR(16)    NULL, -- CALL | PUT | BINARY
    ADD COLUMN exercise_style       VARCHAR(16)    NULL, -- EUROPEAN | AMERICAN
    ADD COLUMN expiry_at            TIMESTAMPTZ    NULL, -- OPTION expiry
    ADD COLUMN barrier_type         VARCHAR(16)    NULL, -- UP_AND_IN/OUT, DOWN_AND_IN/OUT
    ADD COLUMN barrier_level        DECIMAL(20,8)  NULL, -- OPTION barrier level
    ADD COLUMN value_date           DATE           NULL, -- FORWARD value date
    ADD COLUMN near_leg_value_date  DATE           NULL, -- SWAP near leg
    ADD COLUMN far_leg_value_date   DATE           NULL, -- SWAP far leg
    ADD COLUMN premium              DECIMAL(28,8)  NULL, -- OPTION premium amount
    ADD COLUMN premium_currency     VARCHAR(16)    NOT NULL DEFAULT 'QUOTE', -- QUOTE | SETTLEMENT
    ADD COLUMN ndf_fixing_source    VARCHAR(64)    NULL; -- NDF fixing source token

-- Enum + positivity backstops (spec §5.4 vocabularies; application-level
-- validation in services/internal/orders is the primary gate).
ALTER TABLE orders
    ADD CONSTRAINT orders_option_type_chk
        CHECK (option_type IN ('CALL','PUT','BINARY')),
    ADD CONSTRAINT orders_exercise_style_chk
        CHECK (exercise_style IN ('EUROPEAN','AMERICAN')),
    ADD CONSTRAINT orders_barrier_type_chk
        CHECK (barrier_type IN ('UP_AND_IN','UP_AND_OUT','DOWN_AND_IN','DOWN_AND_OUT')),
    ADD CONSTRAINT orders_premium_currency_chk
        CHECK (premium_currency IN ('QUOTE','SETTLEMENT')),
    ADD CONSTRAINT orders_strike_positive_chk
        CHECK (strike IS NULL OR strike > 0),
    ADD CONSTRAINT orders_barrier_level_positive_chk
        CHECK (barrier_level IS NULL OR barrier_level > 0),
    ADD CONSTRAINT orders_premium_positive_chk
        CHECK (premium IS NULL OR premium > 0),
    -- A barrier option must carry BOTH barrier fields; neither may
    -- appear alone.
    ADD CONSTRAINT orders_barrier_pair_chk
        CHECK ((barrier_type IS NULL) = (barrier_level IS NULL)),
    -- SWAP legs must arrive as a pair, far strictly after near.
    ADD CONSTRAINT orders_swap_leg_dates_chk
        CHECK ((near_leg_value_date IS NULL) = (far_leg_value_date IS NULL)
               AND (near_leg_value_date IS NULL OR far_leg_value_date > near_leg_value_date)),
    -- An OPTION expiry before its delivery value date is impossible.
    ADD CONSTRAINT orders_option_expiry_vs_value_chk
        CHECK (expiry_at IS NULL OR value_date IS NULL
               OR expiry_at::date <= value_date);

COMMENT ON COLUMN orders.strike              IS 'OPTION strike, quote per base (spec §5.4).';
COMMENT ON COLUMN orders.option_type         IS 'CALL|PUT|BINARY (spec §5.4).';
COMMENT ON COLUMN orders.exercise_style      IS 'EUROPEAN|AMERICAN (spec §5.4).';
COMMENT ON COLUMN orders.expiry_at           IS 'OPTION expiry timestamp (spec §5.4).';
COMMENT ON COLUMN orders.barrier_type        IS 'UP_AND_IN|UP_AND_OUT|DOWN_AND_IN|DOWN_AND_OUT (spec §5.4).';
COMMENT ON COLUMN orders.barrier_level       IS 'OPTION barrier level (spec §5.4).';
COMMENT ON COLUMN orders.value_date          IS 'FORWARD value date (spec §5.4).';
COMMENT ON COLUMN orders.near_leg_value_date IS 'SWAP near-leg value date (spec §5.4).';
COMMENT ON COLUMN orders.far_leg_value_date  IS 'SWAP far-leg value date (spec §5.4).';
COMMENT ON COLUMN orders.premium             IS 'OPTION premium amount (spec §5.4).';
COMMENT ON COLUMN orders.premium_currency    IS 'QUOTE|SETTLEMENT premium denomination (spec §5.4; VARCHAR(16) so SETTLEMENT fits).';
COMMENT ON COLUMN orders.ndf_fixing_source   IS 'NDF fixing source token — §15.7 central-bank/vendor hierarchy (spec §5.4 note; separate from fixing_benchmark owned by migration 038).';
