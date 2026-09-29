-- 201_unfreeze_requests.up.sql
-- Phase-12 Task 12.3.10 — unfreeze request records for self-frozen
-- (SELF_FREEZE) accounts.
--
-- POST /api/v1/account/unfreeze-request creates a SUBMITTED record:
-- the account owner attests they are requesting re-verification and may
-- attach government-ID / liveness-check document references produced by
-- the Phase-14 KYC pipeline. The actual identity re-verification +
-- 2FA-reset + UNFROZEN transition is an admin workflow owned by Phase-14
-- (Task 14.3.x) — this table is the durable request state machine.
--
-- status: SUBMITTED → DOCS_VERIFIED → UNFROZEN
--                    ↘ REJECTED / CANCELLED (terminal)
-- One open request per account (partial unique index below); a second
-- submission while SUBMITTED/DOCS_VERIFIED replays the open row.

BEGIN;

CREATE TABLE unfreeze_requests (
    id              BIGSERIAL    PRIMARY KEY,
    account_id      BIGINT       NOT NULL REFERENCES accounts (id),
    user_id         BIGINT       NOT NULL REFERENCES users (id),
    status          VARCHAR(16)  NOT NULL DEFAULT 'SUBMITTED'
                    CHECK (status IN ('SUBMITTED','DOCS_VERIFIED','UNFROZEN',
                                      'REJECTED','CANCELLED')),
    id_document_ref VARCHAR(255),                 -- government ID doc reference
    liveness_ref    VARCHAR(255),                 -- liveness-check artifact ref
    note            TEXT,
    decided_by      BIGINT,                       -- admin verifier (Phase-14)
    decided_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- One open request per account.
CREATE UNIQUE INDEX unfreeze_requests_open_uq
    ON unfreeze_requests (account_id)
    WHERE status IN ('SUBMITTED','DOCS_VERIFIED');

CREATE INDEX unfreeze_requests_account_ix
    ON unfreeze_requests (account_id, created_at);

COMMIT;
