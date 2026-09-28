-- 162_chargebacks
-- Phase-05 Wave-2 Cluster B (Task 5.3.18 Chargeback Handling; Phase-05
-- AC #31 "chargeback dispute workflow with account freeze").
--
-- chargebacks is the dispute record a Finance Ops admin opens against a
-- funding transaction / card-not-present deposit; chargeback_evidence is
-- the immutable evidence bundle collected for the processor response
-- (trade records, funding snapshot, freeze events, communications) with
-- a sha256 payload digest per row for tamper evidence.

BEGIN;

CREATE TYPE chargeback_status_enum AS ENUM
    ('DISPUTE_OPENED', 'EVIDENCE_COLLECTED', 'SUBMITTED', 'RESOLVED_WON', 'RESOLVED_LOST');

CREATE TABLE chargebacks (
    id                     BIGSERIAL PRIMARY KEY,
    account_id             BIGINT NOT NULL REFERENCES accounts (id),
    funding_transaction_id BIGINT REFERENCES funding_transactions (id),
    card_network           VARCHAR(32),
    currency               VARCHAR(3) NOT NULL,
    amount                 DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    reason                 TEXT NOT NULL,
    status                 chargeback_status_enum NOT NULL DEFAULT 'DISPUTE_OPENED',
    opened_by              BIGINT NOT NULL,           -- admin user id
    opened_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    submitted_at           TIMESTAMPTZ,
    resolved_at            TIMESTAMPTZ,
    resolution_note        TEXT,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE chargeback_evidence (
    id             BIGSERIAL PRIMARY KEY,
    chargeback_id  BIGINT NOT NULL REFERENCES chargebacks (id) ON DELETE CASCADE,
    kind           VARCHAR(32) NOT NULL,      -- FUNDING_TX | TRADE | FREEZE_EVENT | LEDGER | NOTE
    payload        JSONB NOT NULL,
    payload_sha256 CHAR(64) NOT NULL,
    collected_by   BIGINT,                    -- admin user id (NULL = auto-collected)
    collected_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_chargebacks_account  ON chargebacks (account_id, opened_at DESC);
CREATE INDEX idx_chargebacks_funding  ON chargebacks (funding_transaction_id)
    WHERE funding_transaction_id IS NOT NULL;
CREATE INDEX idx_chargeback_evidence  ON chargeback_evidence (chargeback_id, id);

COMMIT;
