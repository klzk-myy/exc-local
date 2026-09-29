-- 203_kyc_submission.up.sql
-- Phase-12 Task 12.3.4 — KYC submission pipeline (spec §14.2, §24 #102).
--
-- kyc_submissions is the intake record for POST /api/v1/kyc/submit: one
-- row per submission carrying the requested tier, the 24h manual-review
-- SLA deadline (Task 12.3.13) and the re-verification due date computed
-- at approval (verified_at + 12mo T2 / +24mo institutional). kyc_documents
-- (migration 017) gains the submission link + storage-integrity columns
-- (sha256 / size_bytes / sse_algorithm) for the S3 SSE-KMS objects.
--
-- Boundary: accounts.kyc_tier stays 'T0' at submission — Phase-14 Task
-- 14.3.4 owns approve/reject transitions and tier assignment. The
-- kyc_tier_enum (migration 003) carries no INSTITUTIONAL value: an
-- approved institutional account resolves to kyc_tier 'T2' +
-- client_category 'ELIGIBLE_COUNTERPARTY' (Phase-14 Task 14.3.7); the
-- 'INSTITUTIONAL' requested_tier here routes to manual review.
--
-- The §14.2 tier limits are wired as tier-scoped risk_limits rows — the
-- existing enforcement seam (risk.LimitsService.CheckOrder /
-- CheckWithdrawal via funding.WithdrawalService.WithLimits). NOTE on
-- denomination: the limits seam compares transaction-currency amounts
-- (withdrawal currency / quote notional), so the $10K/$100K figures act
-- as USD-par caps — exact for USD, approximate for other fiats until a
-- converted check lands on the Phase-19.5 oracle seam.

BEGIN;

CREATE TABLE kyc_submissions (
    id              BIGSERIAL PRIMARY KEY,
    account_id      BIGINT       NOT NULL REFERENCES accounts (id),
    requested_tier  VARCHAR(16)  NOT NULL
                    CHECK (requested_tier IN ('T1','T2','INSTITUTIONAL')), -- T0 is the unverified default, never requested
    status          VARCHAR(16)  NOT NULL DEFAULT 'PENDING_REVIEW'
                    CHECK (status IN ('PENDING_REVIEW','UNDER_REVIEW','APPROVED',
                                      'REJECTED','APPEALED','FAILED','EXPIRED')),
    jurisdiction    CHAR(2),                            -- ISO 3166-1 alpha-2 applicant country
    risk_score      SMALLINT     NOT NULL DEFAULT 0     -- 0..100 intake score, banded by kyc_tier_policies (migration 204)
                    CHECK (risk_score BETWEEN 0 AND 100),
    submitted_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    sla_due_at      TIMESTAMPTZ  NOT NULL,              -- submitted_at + manual-review SLA (24h per ops matrix)
    verified_at     TIMESTAMPTZ,                        -- set by Phase-14 review (approve)
    reverify_due_at TIMESTAMPTZ,                        -- verified_at + 12mo (T2) / +24mo (INSTITUTIONAL)
    reviewed_at     TIMESTAMPTZ,
    reviewer_id     BIGINT,                             -- admin user id (Phase-14 write path)
    reject_reason   TEXT,
    appeal_of       BIGINT REFERENCES kyc_submissions (id), -- reject-and-appeal chain (Task 12.3.13)
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX kyc_submissions_account_idx ON kyc_submissions (account_id, id DESC);
-- SLA breach feed: ListOverdueReview scans PENDING_REVIEW rows past due.
CREATE INDEX kyc_submissions_sla_idx ON kyc_submissions (sla_due_at)
    WHERE status = 'PENDING_REVIEW';

ALTER TABLE kyc_documents
    ADD COLUMN submission_id BIGINT REFERENCES kyc_submissions (id),
    ADD COLUMN sha256        VARCHAR(64),                   -- hex SHA-256 of stored bytes (integrity)
    ADD COLUMN size_bytes    BIGINT,
    ADD COLUMN sse_algorithm VARCHAR(16) NOT NULL DEFAULT 'aws:kms'; -- §24 #102 SSE-KMS marker
CREATE INDEX kyc_documents_submission_idx ON kyc_documents (submission_id);

-- §14.2 tier caps, tier-scoped risk_limits rows (idempotent seed):
--   T0 → withdrawal disabled + $0 trading   (0 caps reject any positive amount)
--   T1 → $10K/day withdrawal + $10K/day trading
--   T2 → $100K/day withdrawal, trading unlimited (no row cap on volume)
--   INSTITUTIONAL → negotiated — deliberately no tier row; ops set
--   account-scoped rows after manual review (Phase-14).
INSERT INTO risk_limits (tier, daily_withdraw_limit, max_daily_volume)
SELECT 'T0', 0, 0
WHERE NOT EXISTS (SELECT 1 FROM risk_limits
                  WHERE tier = 'T0' AND account_id IS NULL AND symbol IS NULL);
INSERT INTO risk_limits (tier, daily_withdraw_limit, max_daily_volume)
SELECT 'T1', 10000, 10000
WHERE NOT EXISTS (SELECT 1 FROM risk_limits
                  WHERE tier = 'T1' AND account_id IS NULL AND symbol IS NULL);
INSERT INTO risk_limits (tier, daily_withdraw_limit)
SELECT 'T2', 100000
WHERE NOT EXISTS (SELECT 1 FROM risk_limits
                  WHERE tier = 'T2' AND account_id IS NULL AND symbol IS NULL);

COMMIT;
