-- 203_kyc_submission.down.sql

BEGIN;

-- Remove only the tier-scoped global rows this migration seeded (matched
-- on the seeded cap values so operator-tuned rows of other magnitudes
-- are not clobbered).
DELETE FROM risk_limits
WHERE account_id IS NULL AND symbol IS NULL
  AND ((tier = 'T0' AND daily_withdraw_limit = 0     AND max_daily_volume = 0)
    OR (tier = 'T1' AND daily_withdraw_limit = 10000 AND max_daily_volume = 10000)
    OR (tier = 'T2' AND daily_withdraw_limit = 100000 AND max_daily_volume IS NULL));

DROP INDEX IF EXISTS kyc_documents_submission_idx;
ALTER TABLE kyc_documents
    DROP COLUMN IF EXISTS submission_id,
    DROP COLUMN IF EXISTS sha256,
    DROP COLUMN IF EXISTS size_bytes,
    DROP COLUMN IF EXISTS sse_algorithm;

DROP TABLE IF EXISTS kyc_submissions;

COMMIT;
