-- 277_seed_fee_tiers.up.sql
-- Task 3.3.4 went live with the fill-path fee wiring: PgxTradeResolver
-- computes per-side trading fees via FeeService, which fails closed with
-- FEE_TIER_NOT_FOUND when an account has no resolvable tier. fee_tiers
-- (012) ships empty and accounts.fee_tier_id (003/020) was never
-- assigned — this seeds the baseline ladder mirroring vip_tier_schedule
-- (086) retail rates and backfills every unassigned account to STANDARD.
-- Rates are bps of the received side: buyer fee in base, seller in quote.

BEGIN;

INSERT INTO fee_tiers (tier_name, maker_bps, taker_bps)
SELECT v.tier_name, v.maker_bps, v.taker_bps
  FROM (VALUES
        ('STANDARD',      1.0000, 1.5000),  -- VIP 0 baseline
        ('PROFESSIONAL',  0.8000, 1.3000),  -- VIP 2
        ('INSTITUTIONAL', 0.6000, 1.2000))  -- VIP 3
       AS v(tier_name, maker_bps, taker_bps)
 WHERE NOT EXISTS (SELECT 1 FROM fee_tiers f WHERE f.tier_name = v.tier_name);

UPDATE accounts
   SET fee_tier_id = (SELECT id FROM fee_tiers WHERE tier_name = 'STANDARD')
 WHERE fee_tier_id IS NULL;

COMMIT;
