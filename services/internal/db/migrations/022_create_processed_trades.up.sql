-- 022_create_processed_trades.up.sql
-- Phase-03 Task 3.3.1: idempotent trade-fill dedup for the post-trade
-- balance service (spec §5.3/§5.40). This table was missing from the
-- original Phase-01 migration list (001–020) — migration number assigned
-- by Phase-03 Task 3.3.1's migration note.
--
-- One row per engine trade_id consumed by the balance service. The insert
-- rides inside the SAME SERIALIZABLE transaction as the balance journal,
-- so a replayed TradeFill resolves to a no-op, never a double credit.
-- position_fills (migration 111) dedups the position leg separately — the
-- two tables are deliberately independent per their task split.

BEGIN;

CREATE TABLE processed_trades (
    trade_id     BIGINT      PRIMARY KEY,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    shard_id     SMALLINT
);

COMMIT;
