-- 286_recon_dead_letters.up.sql
-- Acknowledged permanently-unresolvable journal entities. The WAL is
-- append-only: a journaled trade or resting order whose projection rows
-- were destroyed can never reach PostgreSQL again, so reconciliation
-- would re-report it as a fresh wal_vs_pg mismatch (and re-arm halts)
-- forever. Writers: the gateway boot recovery scan. Readers: the ORDERS
-- and TRADES reconciliation legs downgrade acknowledged entities from
-- MISMATCH+halt to a counted INFO summary — the gap stays visible, the
-- protection stops re-firing on what cannot be fixed.

BEGIN;

CREATE TABLE recon_dead_letters (
    entity     TEXT        NOT NULL,           -- 'trade' | 'order'
    entity_id  BIGINT      NOT NULL,           -- engine trade_id / order_id
    reason     TEXT        NOT NULL,           -- e.g. 'unresolvable_orders'
    detail     JSONB,
    noted_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (entity, entity_id)
);

COMMIT;
