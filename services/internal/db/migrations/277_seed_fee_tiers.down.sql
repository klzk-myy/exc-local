-- 277_seed_fee_tiers.down.sql

BEGIN;

UPDATE accounts
   SET fee_tier_id = NULL
 WHERE fee_tier_id IN (SELECT id FROM fee_tiers
                        WHERE tier_name IN ('STANDARD','PROFESSIONAL','INSTITUTIONAL'));

DELETE FROM fee_tiers
 WHERE tier_name IN ('STANDARD','PROFESSIONAL','INSTITUTIONAL');

COMMIT;
