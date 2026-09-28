-- 092_recovery_digests.up.sql
-- Phase-04 Task 4.3.11 (spec §18.6.7, §24 #353): running integrity digests.
--
-- One row per shard per checkpoint (every 1,000 trades — see
-- services/internal/recovery/digest.go, OrchDigestInterval). The pre-open
-- audit verifies the digest chain and recomputes only the post-checkpoint
-- window instead of rescanning full history; a digest mismatch falls back
-- to the full scan for that shard only (Task 4.3.12 scoped reopen).
--
--   checkpoint_seq    — trade-count watermark; multiples of 1,000.
--   journal_seq       — journal_entries.id watermark at checkpoint time;
--                       the GL window scan cursor (ledger_lines have no
--                       trade seq, so journal id is the replayable boundary).
--   book_seq          — engine book_seq watermark at the checkpoint
--                       (== WAL tail cursor per the §3.5 sequence domain).
--   gl_zero_sum_hash  — SHA-256 hex of the canonical per-currency
--                       debit/credit sum vector over the checkpoint window
--                       (see OrchGLZeroSumHash).
--   balance_delta_hash— SHA-256 hex of the canonical per-account-code
--                       net-delta vector since the previous checkpoint
--                       (see OrchBalanceDeltaHash).

BEGIN;

CREATE TABLE recovery_digests (
    id                 BIGSERIAL PRIMARY KEY,
    shard_id           SMALLINT     NOT NULL,
    checkpoint_seq     BIGINT       NOT NULL CHECK (checkpoint_seq > 0),
    journal_seq        BIGINT       NOT NULL DEFAULT 0,
    book_seq           BIGINT       NOT NULL DEFAULT 0,
    gl_zero_sum_hash   CHAR(64)     NOT NULL,
    balance_delta_hash CHAR(64)     NOT NULL,
    trade_count        BIGINT       NOT NULL CHECK (trade_count >= 0),
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (shard_id, checkpoint_seq)
);

-- Latest-per-shard probe is the audit hot path (ORDER BY checkpoint_seq DESC
-- LIMIT 1); the unique index above already covers (shard_id, checkpoint_seq)
-- ordering, so no separate index is needed.

COMMIT;
