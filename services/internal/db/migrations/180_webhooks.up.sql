-- 180_webhooks.up.sql
-- Phase-05 Task 5.3.17 — signed outbound webhooks.
--
-- Single owner of the webhook delivery pipeline (ownership pinned
-- 2026-09-27, remediation #35): registration, HMAC-SHA256 signed
-- delivery, bounded exponential-backoff retry and the dead-letter queue
-- all live here. Phase-14 Task 14.3.12's earlier divergent retry
-- schedule and second webhook_dead_letters table are superseded —
-- permanently failed deliveries are queryable as
-- webhook_deliveries.status = 'DEAD_LETTERED'.
--
-- secret_enc is an AES-256-GCM envelope (auth.SecretBox, nonce||ct) of
-- the per-endpoint signing secret — the dispatcher must recover the raw
-- secret to sign, so it is encrypted-not-hashed, same rationale as
-- api_keys.secret_enc (migration 025). Rotation keeps the predecessor in
-- prev_secret_enc valid until secret_overlap_until (bounded ≤72h,
-- mirroring the api_key rotation contract).

BEGIN;

CREATE TABLE webhook_endpoints (
    id                    BIGSERIAL PRIMARY KEY,
    endpoint_id           VARCHAR(32)  NOT NULL UNIQUE,   -- "wh_" + token (public id)
    account_id            BIGINT       NOT NULL REFERENCES accounts (id),
    url                   TEXT         NOT NULL,
    events                TEXT[]       NOT NULL,          -- order_filled|order_cancelled|deposit_confirmed|withdrawal_completed
    secret_enc            BYTEA        NOT NULL,          -- sealed HMAC-SHA256 signing secret
    prev_secret_enc       BYTEA,                          -- predecessor during rotation overlap
    secret_overlap_until  TIMESTAMPTZ,                    -- predecessor delivery-verify grace
    status                VARCHAR(16)  NOT NULL DEFAULT 'ACTIVE',  -- ACTIVE | DISABLED
    created_by            BIGINT,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    disabled_at           TIMESTAMPTZ,
    CONSTRAINT webhook_endpoints_url_https CHECK (url ~ '^https?://[^[:space:]]+$')
);

CREATE INDEX idx_webhook_endpoints_account ON webhook_endpoints (account_id)
    WHERE status = 'ACTIVE';

-- Deliveries double as the delivery log (per-attempt bookkeeping) and the
-- dead-letter queue (status = 'DEAD_LETTERED' after max_attempts).
CREATE TABLE webhook_deliveries (
    id                BIGSERIAL PRIMARY KEY,
    delivery_id       VARCHAR(32)  NOT NULL UNIQUE,       -- "whd_" + token
    endpoint_id       BIGINT       NOT NULL REFERENCES webhook_endpoints (id),
    account_id        BIGINT       NOT NULL,
    event             VARCHAR(64)  NOT NULL,
    payload           JSONB        NOT NULL,
    status            VARCHAR(16)  NOT NULL DEFAULT 'PENDING', -- PENDING | DELIVERED | DEAD_LETTERED
    attempts          INTEGER      NOT NULL DEFAULT 0,
    max_attempts      INTEGER      NOT NULL DEFAULT 5,
    next_attempt_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    last_status_code  INTEGER,
    last_error        TEXT,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    delivered_at      TIMESTAMPTZ,
    CONSTRAINT webhook_deliveries_attempts_nonneg CHECK (attempts >= 0),
    CONSTRAINT webhook_deliveries_max_pos        CHECK (max_attempts > 0)
);

-- Dispatcher scans due work; PENDING-only partial index keeps it small.
CREATE INDEX idx_webhook_deliveries_due ON webhook_deliveries (next_attempt_at)
    WHERE status = 'PENDING';
CREATE INDEX idx_webhook_deliveries_account ON webhook_deliveries (account_id, id DESC);
-- Dead-letter review surface.
CREATE INDEX idx_webhook_deliveries_dead ON webhook_deliveries (endpoint_id)
    WHERE status = 'DEAD_LETTERED';

COMMIT;
