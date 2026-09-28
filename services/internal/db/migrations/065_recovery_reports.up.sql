-- 065_recovery_reports.up.sql
-- Phase-04 Task 4.3.5 / 4.3.9 — graduated WAL recovery ladder diagnostics
-- (spec §3.5/§18.1/§18.5). One immutable row per ladder run that needed
-- operator-visible action: WAL_REPAIRED / SNAPSHOT_REBASED audit records and
-- the WAL_RECOVERY_HALT fail-closed halt (the Level-3 last resort that pages
-- P1 and holds the shard in MarketDataOnly pending dual-control).
--
-- Written by: the C++ recovery manager's JSONL emitter
-- (recovery_report.jsonl -> services/cmd/wal-recovery persist-reports) and the
-- daily ledger reconciliation job (internal/recovery, stage='daily_reconcile').
-- Append-only: corrections land as new rows, never UPDATE/DELETE.

BEGIN;

CREATE TABLE recovery_reports (
    id                  BIGSERIAL PRIMARY KEY,
    shard_id            SMALLINT     NOT NULL,
    book_seq            BIGINT       NOT NULL DEFAULT -1,  -- recomputed cursor; -1 unknown
    wal_tail            BIGINT       NOT NULL DEFAULT 0,   -- last valid seq + 1
    last_valid_seq      BIGINT       NOT NULL DEFAULT -1,  -- last CRC-valid entry seq
    snapshot_seq        BIGINT       NOT NULL DEFAULT 0,   -- WAL cursor the snapshot covers
    first_divergent_seq BIGINT       NOT NULL DEFAULT -1,  -- first unverifiable seq
    stage               TEXT         NOT NULL,             -- 'boot_ladder' | 'daily_reconcile' | ...
    outcome             TEXT         NOT NULL,             -- CLEAN | WAL_REPAIRED | SNAPSHOT_REBASED | WAL_RECOVERY_HALT | RECONCILE_*
    detail              JSONB        NOT NULL DEFAULT '{}'::jsonb,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX recovery_reports_shard_ix   ON recovery_reports (shard_id, id);
CREATE INDEX recovery_reports_outcome_ix ON recovery_reports (outcome);
CREATE INDEX recovery_reports_stage_ix   ON recovery_reports (stage, created_at);

COMMIT;
