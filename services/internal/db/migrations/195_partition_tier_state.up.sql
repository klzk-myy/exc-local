-- 195_partition_tier_state.up.sql
-- Phase-09 Task 9.3.24 — hot/warm/cold data tiering: per-partition tier
-- bookkeeping and the transition log (spec §19.7, §19.12).
--
-- partition_tier_state is the scheduler's source of truth: the lifecycle
-- runner writes WARM after detach-to-warm-schema and COLD after the WORM
-- upload is verified and the local table dropped. HOT rows exist only for
-- partitions that have been touched by the scheduler (untouched children
-- are implicitly HOT).
--
-- partition_tier_log is the append-only transition history — every move
-- records row-count/checksum verification evidence.

BEGIN;

CREATE TABLE partition_tier_state (
    parent_table    VARCHAR(128) NOT NULL,
    partition_name  VARCHAR(128) NOT NULL,
    tier            VARCHAR(8)   NOT NULL
                    CHECK (tier IN ('HOT','WARM','COLD')),
    tier_since      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    range_end       TIMESTAMPTZ,            -- partition TO bound; drives warm aging
    bound_expr      TEXT,                   -- raw relpartbound for reattach
    warm_schema     VARCHAR(128),           -- set while tier='WARM'
    archive_id      BIGINT REFERENCES partition_archive_log(archive_id),
    row_count       BIGINT,
    checksum        CHAR(64),               -- sha256 evidence for the move
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (parent_table, partition_name)
);

CREATE TABLE partition_tier_log (
    log_id          BIGSERIAL    PRIMARY KEY,
    run_id          UUID         NOT NULL,
    parent_table    VARCHAR(128) NOT NULL,
    partition_name  VARCHAR(128) NOT NULL,
    from_tier       VARCHAR(8),
    to_tier         VARCHAR(8)   NOT NULL,
    result          VARCHAR(16)  NOT NULL
                    CHECK (result IN ('MOVED','HELD','SKIPPED','ERROR')),
    row_count       BIGINT,
    checksum        CHAR(64),
    detail          JSONB,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX partition_tier_log_part_idx
    ON partition_tier_log (parent_table, partition_name, created_at);

COMMIT;
