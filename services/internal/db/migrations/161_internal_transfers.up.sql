-- 161_internal_transfers
-- Phase-05 Wave-2 Cluster B (Task 5.3.23 Internal Transfers + Task 5.3.45
-- Transfer History Query; spec §8.8 idempotency, §24 #144/#363).
--
-- transfers is the client-visible journal for intra-venue money movement
-- between caller-owned accounts (same user_id, incl. master↔sub since
-- sub-accounts inherit the master's user_id). account_id is the
-- idempotency namespace owner (= from_account_id for SELF-initiated
-- moves); actor records self vs admin vs system sweep so the history
-- endpoint can disclose who moved the funds.

BEGIN;

CREATE TYPE transfer_status_enum AS ENUM ('PENDING', 'COMPLETED', 'FAILED');
CREATE TYPE transfer_actor_enum  AS ENUM ('SELF', 'ADMIN', 'SYSTEM');

CREATE TABLE transfers (
    id               BIGSERIAL PRIMARY KEY,
    account_id       BIGINT NOT NULL REFERENCES accounts (id),  -- initiating account (idem namespace)
    from_account_id  BIGINT NOT NULL REFERENCES accounts (id),
    to_account_id    BIGINT NOT NULL REFERENCES accounts (id)
        CHECK (from_account_id <> to_account_id),
    currency         VARCHAR(3)  NOT NULL,
    amount           DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    status           transfer_status_enum NOT NULL DEFAULT 'PENDING',
    actor            transfer_actor_enum  NOT NULL DEFAULT 'SELF',
    actor_id         BIGINT,                                    -- user/admin id behind the move
    journal_entry_id BIGINT,                                    -- GL reference (journal_entries.id)
    failure_reason   TEXT,
    idempotency_key  VARCHAR(128),
    payload_sha256   CHAR(64),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ
);

-- Account-scoped safe-retry (spec §8.8).
CREATE UNIQUE INDEX uq_transfers_idem
    ON transfers (account_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- History scans (Task 5.3.45): cursor on (created_at, id) per endpoint.
CREATE INDEX idx_transfers_from ON transfers (from_account_id, created_at DESC, id DESC);
CREATE INDEX idx_transfers_to   ON transfers (to_account_id,   created_at DESC, id DESC);

COMMIT;
