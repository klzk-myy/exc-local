-- 199_funding_flow_extensions.up.sql
-- Phase-11 flows cluster — Tasks 11.3.2 (withdrawal lifecycle), 11.3.3
-- (deposit lifecycle + anti-fraud tiers), 11.3.6 (nostro-aware
-- dispatch), 11.3.10 support tables.
--
-- (Migration number: 198 was already claimed by parallel Phase-11
-- clusters — this is the next free number ≥198. nostro_accounts.balance
-- (018) already carries the per-currency nostro balance, so no separate
-- nostro_balances table is created.)
--
-- Contents:
--   1. funding_transactions review/hold columns — hold_until (24h
--      unverified-destination hold, Task 11.3.2 step 6) and the
--      reviewer attribution for admin approve/reject plus
--      originator_name for deposit source-of-funds checks.
--   2. deposit_confirmations — dual-source bank verification ledger
--      (Task 11.3.3: two INDEPENDENT confirmations before credit).
--   3. withdrawal_dispatch_queue — CONFIRMED withdrawals held for
--      nostro headroom or destination hold; a queued withdrawal is
--      never rejected on insufficient nostro (Task 11.3.6).
--   4. funding_ops_alerts — durable ops-alert trail for the funding
--      domain (nostro shortfall, dispatch/replenishment failures);
--      complements the NATS ops.alerts.* page stream.
--   5. nostro_replenishment_requests — dual-controlled reserve→nostro
--      movement requests (Task 11.3.6 step 2/3).
--   6. withdrawal_destination_holds — first-seen registry for
--      destinations outside the verified beneficiary registry; the
--      account-scoped 24h unverified-destination hold (Task 11.3.2
--      step 6).

BEGIN;

ALTER TABLE funding_transactions
    ADD COLUMN IF NOT EXISTS hold_until      TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS reviewed_by     BIGINT,
    ADD COLUMN IF NOT EXISTS reviewed_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS originator_name VARCHAR(255);

-- Dispatch scan: CONFIRMED withdrawals in hold/release order.
CREATE INDEX IF NOT EXISTS funding_tx_dispatch_ix
    ON funding_transactions (hold_until, id)
    WHERE type = 'WITHDRAWAL' AND status = 'CONFIRMED';

-- Deposit review scan.
CREATE INDEX IF NOT EXISTS funding_tx_deposit_review_ix
    ON funding_transactions (review_deadline)
    WHERE type = 'DEPOSIT' AND status = 'PENDING_REVIEW';

CREATE TABLE deposit_confirmations (
    id                     BIGSERIAL PRIMARY KEY,
    funding_transaction_id BIGINT      NOT NULL REFERENCES funding_transactions (id),
    source                 VARCHAR(48) NOT NULL,           -- STATEMENT | WEBHOOK | CAMT054 | MT103 | ...
    sender_name            VARCHAR(255),                   -- originator name reported by this source
    sender_account         VARCHAR(64),
    payload_sha256         CHAR(64),                       -- dedup: same source+same payload replays
    received_by            BIGINT,                         -- ops user recording the confirmation
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Dual-source contract: one row per (deposit, source); the second
    -- DISTINCT source completes dual-source verification.
    UNIQUE (funding_transaction_id, source)
);

CREATE TABLE withdrawal_dispatch_queue (
    id            BIGSERIAL PRIMARY KEY,
    withdrawal_id BIGINT      NOT NULL UNIQUE REFERENCES funding_transactions (id),
    status        VARCHAR(16) NOT NULL DEFAULT 'QUEUED'
                  CHECK (status IN ('QUEUED','DISPATCHED','CANCELLED')),
    reason        VARCHAR(48) NOT NULL,                    -- NOSTRO_INSUFFICIENT | DESTINATION_HOLD | HOLD_RELEASE
    queued_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    dispatched_at TIMESTAMPTZ,
    attempts      INT         NOT NULL DEFAULT 0,
    last_error    TEXT
);

CREATE INDEX wdq_due_ix ON withdrawal_dispatch_queue (queued_at)
    WHERE status = 'QUEUED';

CREATE TABLE funding_ops_alerts (
    id                     BIGSERIAL PRIMARY KEY,
    code                   VARCHAR(64) NOT NULL,           -- e.g. NOSTRO_INSUFFICIENT_FUNDS
    severity               VARCHAR(4)  NOT NULL DEFAULT 'P1'
                           CHECK (severity IN ('P0','P1','P2','P3')),
    funding_transaction_id BIGINT REFERENCES funding_transactions (id),
    account_id             BIGINT,
    currency               VARCHAR(3),
    amount                 DECIMAL(28,8),
    summary                TEXT NOT NULL,
    detail                 JSONB,
    status                 VARCHAR(12) NOT NULL DEFAULT 'OPEN'
                           CHECK (status IN ('OPEN','ACKED','RESOLVED')),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at            TIMESTAMPTZ
);

CREATE INDEX funding_ops_alerts_open_ix
    ON funding_ops_alerts (created_at) WHERE status = 'OPEN';

CREATE TABLE nostro_replenishment_requests (
    id               BIGSERIAL PRIMARY KEY,
    currency         VARCHAR(3)    NOT NULL,
    amount           DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    source_nostro_id BIGINT        NOT NULL REFERENCES nostro_accounts (id), -- reserve
    target_nostro_id BIGINT        NOT NULL REFERENCES nostro_accounts (id), -- operating
    status           VARCHAR(16)   NOT NULL DEFAULT 'PENDING_APPROVAL'
                     CHECK (status IN ('PENDING_APPROVAL','EXECUTED','REJECTED')),
    requested_by     BIGINT,                               -- NULL = auto-requested by dispatcher
    approved_by      BIGINT,                               -- dual control: must differ from requested_by
    decision_note    TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at       TIMESTAMPTZ,

    CONSTRAINT nostro_replen_distinct CHECK (source_nostro_id <> target_nostro_id)
);

CREATE TABLE withdrawal_destination_holds (
    id           BIGSERIAL PRIMARY KEY,
    account_id   BIGINT       NOT NULL REFERENCES accounts (id),
    destination  VARCHAR(64)  NOT NULL,                    -- normalized (upper) IBAN / account ref
    first_seen   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    unlocked_at  TIMESTAMPTZ  NOT NULL,                    -- first_seen + 24h
    UNIQUE (account_id, destination)
);

COMMIT;
