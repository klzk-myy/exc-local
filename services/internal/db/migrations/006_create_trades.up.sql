-- 006_create_trades.up.sql
-- Spec §5.5 trades — daily native declarative partitioning by created_at,
-- managed by pg_partman (extension pre-installed in schema public).
-- Excluded (owned by later migrations): status (051 — trade bust lifecycle).
--
-- NOTE (pg_partman 5.x): create_parent signature is
--   (p_parent_table, p_control, p_interval, p_type DEFAULT 'range', ...).
-- The 'native' positional argument used in pg_partman 4.x no longer exists —
-- all partitioning is native in 5.x — and the legacy interval keywords
-- ('daily' etc.) were removed in 5.5: p_interval takes a core PostgreSQL
-- interval ('1 day').
-- PK must include the partition key: PRIMARY KEY (id, created_at), with id a
-- plain BIGINT GENERATED ALWAYS AS IDENTITY. Indexes/FKs on the parent
-- propagate to partitions; rows route by created_at.

BEGIN;

CREATE TABLE trades (
    id                BIGINT GENERATED ALWAYS AS IDENTITY,
    instrument_id     BIGINT NOT NULL,
    buy_order_id      BIGINT NOT NULL,
    sell_order_id     BIGINT NOT NULL,
    buyer_account_id  BIGINT NOT NULL,
    seller_account_id BIGINT NOT NULL,
    price             DECIMAL(20,8) NOT NULL,
    quantity          DECIMAL(28,8) NOT NULL,
    buyer_fee         DECIMAL(20,8),
    seller_fee        DECIMAL(20,8),
    settlement_date   DATE,                              -- T+1 / same-day from trade date
    shard_id          SMALLINT,
    trade_seq         BIGINT,                            -- per-shard sequence
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

-- Register with pg_partman: daily partitions, 30 pre-made ahead, plus a
-- default partition (p_default_table defaults true) as a fail-safe catch-all.
SELECT public.create_parent(
    p_parent_table := 'public.trades',
    p_control      := 'created_at',
    p_interval     := '1 day',
    p_premake      := 30
);

COMMIT;
