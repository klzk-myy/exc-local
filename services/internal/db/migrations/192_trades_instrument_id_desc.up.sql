-- 192_trades_instrument_id_desc.up.sql
-- Phase-08 Task 8.3.3 — performance tuning: hot-query index.
--
-- orders.PgStore.ReferencePrice runs
--   SELECT price FROM trades WHERE instrument_id = $1 ORDER BY id DESC LIMIT 1
-- per order-ingress validation path. trades is RANGE-partitioned by
-- created_at with pkey (id, created_at); the planner therefore does an
-- Index Scan Backward per partition filtering instrument_id post-hoc.
-- Measured on the migverify scratch DB (300k rows in today's partition):
-- a sparse instrument (last trade 300k rows deep) cost 17.3 ms; with this
-- index the same query is 0.24 ms (73x). Per-partition fan-out remains —
-- 31 partitions × one O(1) probe each.
--
-- Created on the partitioned parent: PostgreSQL propagates to every
-- existing partition and auto-creates it on future daily partitions.

CREATE INDEX idx_trades_instrument_id_desc
    ON trades (instrument_id, id DESC);
