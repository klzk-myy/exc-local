-- 160_funding_extensions
-- Phase-05 Wave-2 Cluster B (Tasks 5.3.6, 5.3.23, 5.3.45; spec §5.6/§5.7/§8.8).
--
-- 1. funding_transactions gains the account-scoped idempotency contract
--    (spec §8.8: composite unique (account_id, idempotency_key)) plus the
--    payload hash used to distinguish replay from key reuse, and the
--    canonical review-tier columns (<$10K auto / $10K–$50K standard /
--    >$50K PENDING_REVIEW + 4h review window).
-- 2. withdrawal_confirmations gains token_hash (sha256 hex of the
--    emailed/SMS confirmation token). The 15-minute expires_at default
--    already exists from migration 008.

BEGIN;

ALTER TABLE funding_transactions
    ADD COLUMN IF NOT EXISTS idempotency_key VARCHAR(128),
    ADD COLUMN IF NOT EXISTS payload_sha256   CHAR(64),
    ADD COLUMN IF NOT EXISTS usd_amount       DECIMAL(28,8),
    ADD COLUMN IF NOT EXISTS review_tier      VARCHAR(16),   -- AUTO | STANDARD | PENDING_REVIEW
    ADD COLUMN IF NOT EXISTS review_deadline  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS updated_at       TIMESTAMPTZ NOT NULL DEFAULT now();

-- NULL keys are unconstrained: a missing Idempotency-Key header still
-- executes (the §8.8 "required" gate lands with Task 5.3.42 middleware).
CREATE UNIQUE INDEX IF NOT EXISTS uq_funding_tx_idem
    ON funding_transactions (account_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

ALTER TABLE withdrawal_confirmations
    ADD COLUMN IF NOT EXISTS token_hash CHAR(64);

COMMIT;
