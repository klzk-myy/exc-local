-- 194_data_retention_holds.up.sql
-- Phase-09 Tasks 9.3.17 / 9.3.22 / 9.3.24 — compliance (legal) holds on
-- partitions and the unified retention-enforcer audit trail (spec §19.7,
-- §19.12, §24 #179/#212).
--
-- data_retention_holds: an ACTIVE hold (released_at IS NULL and not
-- expired) blocks every destructive lifecycle transition for the covered
-- scope — detach-to-warm, warm-to-cold drop, and retention purge.
-- partition_name NULL = the hold covers the whole parent table.
--
-- retention_audit_log: one row per enforcer/drill check outcome so the
-- nightly run itself leaves evidence (spec §19.12 item 4 — enforcement
-- must be auditable). RETENTION_POLICY_VIOLATION drift surfaces as
-- result='VIOLATION' rows keyed by run_id.

BEGIN;

CREATE TABLE data_retention_holds (
    hold_id         BIGSERIAL    PRIMARY KEY,
    parent_table    VARCHAR(128) NOT NULL,
    partition_name  VARCHAR(128),             -- NULL = all partitions of parent
    case_ref        VARCHAR(128) NOT NULL,    -- e.g. regulator case / SAR id
    reason          TEXT         NOT NULL,
    created_by      VARCHAR(128) NOT NULL,    -- compliance officer identity
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ,              -- NULL = indefinite hold
    released_at     TIMESTAMPTZ,              -- NULL while hold is active
    released_by     VARCHAR(128)
);

CREATE INDEX data_retention_holds_active_idx
    ON data_retention_holds (parent_table, partition_name)
    WHERE released_at IS NULL;

CREATE TABLE retention_audit_log (
    audit_id        BIGSERIAL    PRIMARY KEY,
    run_id          UUID         NOT NULL,
    mode            VARCHAR(8)   NOT NULL CHECK (mode IN ('DRY_RUN','APPLY')),
    data_type       VARCHAR(64)  NOT NULL,    -- policy class name
    target          VARCHAR(256) NOT NULL,    -- table / partition / object
    check_name      VARCHAR(64)  NOT NULL,    -- e.g. hot_window, worm_integrity
    result          VARCHAR(16)  NOT NULL
                    CHECK (result IN ('OK','VIOLATION','ACTION','HELD',
                                      'SKIPPED','ERROR')),
    detail          JSONB,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX retention_audit_log_run_idx ON retention_audit_log (run_id);
CREATE INDEX retention_audit_log_type_idx
    ON retention_audit_log (data_type, created_at);
CREATE INDEX retention_audit_log_violation_idx
    ON retention_audit_log (created_at) WHERE result = 'VIOLATION';

COMMIT;
