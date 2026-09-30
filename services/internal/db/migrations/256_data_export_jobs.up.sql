-- 256_data_export_jobs.up.sql
-- Phase-23 Task 23.3.2 — market-data export jobs (spec §10, §16.6).
--
--   export_jobs — one row per async export request (CSV / JSON / Parquet
--                 over the ClickHouse trades/ticks/klines projections).
--                 PENDING rows are claimed by the export worker with
--                 SELECT ... FOR UPDATE SKIP LOCKED; the worker streams
--                 keyset pages into an S3 object, then stamps COMPLETED
--                 with the object ref, sha256 and a 24h download expiry.
--                 FAILED is terminal (error text retained for the job
--                 status endpoint). notified_at records the "download
--                 link emailed" fan-out — NULL means the link mail still
--                 owes the account a send (the sweep re-notifies).
--
-- row_limit is the caller-requested bound; the service rejects >1M at
-- admission (EXPORT_LIMIT_EXCEEDED) so persisted rows always satisfy
-- row_limit <= 1000000. row_count is the emitted count; truncated=true
-- means the underlying range still had rows beyond the bound.

BEGIN;

CREATE TABLE export_jobs (
    id           BIGSERIAL    PRIMARY KEY,
    account_id   BIGINT       NOT NULL,
    kind         VARCHAR(8)   NOT NULL CHECK (kind IN ('trades', 'ticks', 'klines')),
    symbol       VARCHAR(32)  NOT NULL,
    interval     VARCHAR(8)   NOT NULL DEFAULT '',      -- klines only
    format       VARCHAR(8)   NOT NULL CHECK (format IN ('csv', 'json', 'parquet')),
    from_ts      TIMESTAMPTZ,                           -- inclusive lower bound; NULL = unbounded
    to_ts        TIMESTAMPTZ,                           -- exclusive upper bound; NULL = unbounded
    row_limit    INTEGER      NOT NULL CHECK (row_limit BETWEEN 1 AND 1000000),
    status       VARCHAR(12)  NOT NULL DEFAULT 'PENDING' CHECK (status IN (
        'PENDING', 'RUNNING', 'COMPLETED', 'FAILED')),
    object_ref   VARCHAR(512),                          -- S3 key of the rendered artifact
    row_count    BIGINT,
    sha256       CHAR(64),                              -- sha256 hex of the artifact bytes
    truncated    BOOLEAN      NOT NULL DEFAULT FALSE,   -- range held more rows than row_limit
    error        TEXT,
    expires_at   TIMESTAMPTZ,                           -- download-link expiry (created + 24h)
    notified_at  TIMESTAMPTZ,                           -- link email dispatched
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ
);

CREATE INDEX ix_export_jobs_account
    ON export_jobs (account_id, created_at DESC, id DESC);

-- Worker claim set: PENDING rows in FIFO order (SKIP LOCKED scan);
-- plus the re-notify scan for COMPLETED rows missing notified_at.
CREATE INDEX ix_export_jobs_pending
    ON export_jobs (id) WHERE status = 'PENDING';

CREATE INDEX ix_export_jobs_unnotified
    ON export_jobs (id) WHERE status = 'COMPLETED' AND notified_at IS NULL;

COMMIT;
