-- 154_order_idempotency.up.sql
-- Phase-05 Task 5.3.24 / Task 5.3.42 — durable idempotency primitives.
--
-- client_order_id_dedup is the spec §5.4a table verbatim (account_id +
-- client_order_id PK, order_id FK, created_at with 24h TTL purge)
-- extended by request_hash: §5.4a requires distinguishing "matching
-- payload → replay ack" from "mismatched payload →
-- IDEMPOTENCY_KEY_COLLISION", which is impossible without a stored
-- fingerprint of the original request. The hash is sha256 over the
-- canonical submit fields — never a raw body — so semantically equal
-- requests collide even if JSON formatting differs.
--
-- idempotency_keys is the account-scoped §8.8 store for the
-- Idempotency-Key header surface (money-moving POSTs incl.
-- /orders/batch): same key + same payload replays the stored ack; same
-- key + different payload → IDEMPOTENCY_KEY_MISMATCH (HTTP 422).

BEGIN;

CREATE TABLE client_order_id_dedup (
    account_id      BIGINT       NOT NULL REFERENCES accounts (id),
    client_order_id VARCHAR(64)  NOT NULL,
    order_id        BIGINT       NOT NULL REFERENCES orders (id),
    request_hash    CHAR(64)     NOT NULL,   -- sha256 hex of canonical submit payload
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, client_order_id)
);

-- §5.4a TTL 24h auto-purge helper index (pg_cron job lives with the
-- retention-policy owner; the index keeps the DELETE cheap).
CREATE INDEX idx_coid_dedup_created ON client_order_id_dedup (created_at);

CREATE TABLE idempotency_keys (
    account_id      BIGINT       NOT NULL REFERENCES accounts (id),
    idempotency_key VARCHAR(64)  NOT NULL,
    endpoint        VARCHAR(128) NOT NULL,              -- e.g. "POST /api/v1/orders/batch"
    request_hash    CHAR(64)     NOT NULL,              -- sha256 hex of canonical request body
    status_code     SMALLINT     NOT NULL,
    response_body   JSONB        NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, idempotency_key)           -- §8.8 account-scoped namespacing
);

CREATE INDEX idx_idem_keys_created ON idempotency_keys (created_at);      -- 24h window purge

COMMIT;
