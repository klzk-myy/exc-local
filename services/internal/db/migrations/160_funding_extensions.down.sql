-- 160_funding_extensions — revert Phase-05 Wave-2 Cluster B funding columns.
BEGIN;

ALTER TABLE withdrawal_confirmations
    DROP COLUMN IF EXISTS token_hash;

DROP INDEX IF EXISTS uq_funding_tx_idem;

ALTER TABLE funding_transactions
    DROP COLUMN IF EXISTS idempotency_key,
    DROP COLUMN IF EXISTS payload_sha256,
    DROP COLUMN IF EXISTS usd_amount,
    DROP COLUMN IF EXISTS review_tier,
    DROP COLUMN IF EXISTS review_deadline,
    DROP COLUMN IF EXISTS updated_at;

COMMIT;
