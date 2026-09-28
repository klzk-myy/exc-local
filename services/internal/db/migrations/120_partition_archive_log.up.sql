-- 120_partition_archive_log.up.sql
-- Phase-04 Task 4.3.7 — PostgreSQL partition archival engine (spec §19.7,
-- §24 #179). Durable audit log for the detach → export → WORM-upload →
-- verify → drop pipeline; restore drills read it back to locate archives.
--
-- status transitions: EXPORTED (uploaded, ETag+sha256 pending verify)
--   → VERIFIED (manifest+etag confirmed) → DROPPED (local partition gone)
--   → RESTORED (drill completed with row-count parity). Any stage may land
--   in FAILED with `error` populated — the partition is never dropped from
--   a FAILED row's pipeline run.

BEGIN;

CREATE TABLE partition_archive_log (
    archive_id        BIGSERIAL PRIMARY KEY,
    parent_table      VARCHAR(128) NOT NULL,
    partition_name    VARCHAR(128) NOT NULL,
    s3_bucket         VARCHAR(255) NOT NULL,
    s3_key            VARCHAR(1024) NOT NULL,
    manifest_key      VARCHAR(1024) NOT NULL,
    row_count         BIGINT       NOT NULL,
    size_bytes        BIGINT       NOT NULL,
    archive_sha256    CHAR(64)     NOT NULL,  -- sha256 of uploaded .zst blob
    csv_sha256        CHAR(64)     NOT NULL,  -- sha256 of uncompressed CSV
    etag              VARCHAR(64)  NOT NULL,  -- S3 ETag = md5 (verified)
    format            VARCHAR(32)  NOT NULL DEFAULT 'csv+zstd',
    object_lock_mode  VARCHAR(16)  NOT NULL DEFAULT 'COMPLIANCE',
    retain_until      TIMESTAMPTZ,
    status            VARCHAR(16)  NOT NULL
                      CHECK (status IN ('EXPORTED','VERIFIED','DROPPED',
                                        'RESTORED','FAILED')),
    archived_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    dropped_at        TIMESTAMPTZ,
    restored_at       TIMESTAMPTZ,
    error             TEXT,
    UNIQUE (s3_bucket, s3_key)
);

CREATE INDEX partition_archive_log_partition_idx
    ON partition_archive_log (partition_name, status);
CREATE INDEX partition_archive_log_parent_idx
    ON partition_archive_log (parent_table, archived_at);

COMMIT;
