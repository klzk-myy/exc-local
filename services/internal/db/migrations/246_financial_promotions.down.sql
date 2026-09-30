-- 246_financial_promotions.down.sql — reverses Task 21.3.26.

BEGIN;

DROP TRIGGER IF EXISTS trg_fp_versions_guard ON financial_promotion_versions;
DROP FUNCTION IF EXISTS financial_promotion_versions_guard();

DROP TABLE IF EXISTS financial_promotion_versions;
DROP TABLE IF EXISTS financial_promotions;

COMMIT;
