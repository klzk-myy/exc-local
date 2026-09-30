-- Migration 039 down — remove ONLY objects owned by 039.
-- orders.fixing_benchmark is owned by migration 038 and is untouched.

ALTER TABLE orders
    DROP CONSTRAINT IF EXISTS orders_option_expiry_vs_value_chk,
    DROP CONSTRAINT IF EXISTS orders_swap_leg_dates_chk,
    DROP CONSTRAINT IF EXISTS orders_barrier_pair_chk,
    DROP CONSTRAINT IF EXISTS orders_premium_positive_chk,
    DROP CONSTRAINT IF EXISTS orders_barrier_level_positive_chk,
    DROP CONSTRAINT IF EXISTS orders_strike_positive_chk,
    DROP CONSTRAINT IF EXISTS orders_premium_currency_chk,
    DROP CONSTRAINT IF EXISTS orders_barrier_type_chk,
    DROP CONSTRAINT IF EXISTS orders_exercise_style_chk,
    DROP CONSTRAINT IF EXISTS orders_option_type_chk;

ALTER TABLE orders
    DROP COLUMN IF EXISTS ndf_fixing_source,
    DROP COLUMN IF EXISTS premium_currency,
    DROP COLUMN IF EXISTS premium,
    DROP COLUMN IF EXISTS far_leg_value_date,
    DROP COLUMN IF EXISTS near_leg_value_date,
    DROP COLUMN IF EXISTS value_date,
    DROP COLUMN IF EXISTS barrier_level,
    DROP COLUMN IF EXISTS barrier_type,
    DROP COLUMN IF EXISTS expiry_at,
    DROP COLUMN IF EXISTS exercise_style,
    DROP COLUMN IF EXISTS option_type,
    DROP COLUMN IF EXISTS strike;
