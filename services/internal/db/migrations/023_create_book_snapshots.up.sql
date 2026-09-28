-- 023_create_book_snapshots.up.sql
-- Phase-04 Task 4.3.1: durable home for matching-engine book snapshots
-- (spec §3.5). This table was missing from the original Phase-01 migration
-- list (001–020) — migration number assigned by the task's migration note.
--
-- One row per (shard, instrument, snapshot_seq). snapshot_data is the C++
-- FlatBuffers-style book blob (WalBookSnapshotHeader + levels + orders +
-- extension block — see core/include/recovery/SnapshotStore.hpp); the
-- Go recovery service (services/internal/recovery) streams it from the
-- shared snap dir after the engine's SnapReadyMsg notification and
-- verifies byte size + CRC32C before insert. Balances are deliberately
-- NOT in this blob — they are reconstructed from WAL trade events by the
-- balance service (boundary pinned 2026-09-27).
--
-- snapshot_seq is the WAL tail cursor the snapshot covers: entries with
-- seq < snapshot_seq are reflected in the blob, replay resumes at
-- seq == snapshot_seq, and the engine's ack consumer uses the confirmed
-- seq to trim sealed WAL segments below it.
--
-- The UNIQUE constraint makes the writer idempotent: a redelivered
-- SnapReadyMsg or a dir-scan catch-up re-INSERT resolves to a no-op.

BEGIN;

CREATE TABLE book_snapshots (
    snapshot_id   BIGSERIAL   PRIMARY KEY,
    shard_id      SMALLINT    NOT NULL,
    instrument_id INTEGER     NOT NULL,
    snapshot_seq  BIGINT      NOT NULL,
    snapshot_data BYTEA       NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (shard_id, instrument_id, snapshot_seq)
);

COMMIT;
