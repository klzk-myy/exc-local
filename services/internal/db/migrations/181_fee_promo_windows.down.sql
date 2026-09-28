-- 181_fee_promo_windows.down.sql

BEGIN;

DROP TABLE IF EXISTS fee_promo_windows;

-- Restore the migration-012 column widths (fails if a live row carries a
-- promo rate above 99.9999 bps — the pre-181 range).
ALTER TABLE fee_tiers
    ALTER COLUMN promo_maker_bps TYPE DECIMAL(6,4),
    ALTER COLUMN promo_taker_bps TYPE DECIMAL(6,4);

COMMIT;
