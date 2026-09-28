-- 155_order_pipeline_columns.up.sql
-- Phase-05 Wave-2 cluster A order-pipeline columns.
--
-- orders.order_seq        — STALE_MODIFY fence (Task 5.3.22 / spec §6.9):
--                           the ingress sequence of the order's last
--                           applied mutation; a modify must carry the
--                           current value or be rejected 409.
-- orders.quote_quantity   — Task 5.3.39 quote-denominated market orders:
--                           the original client ask is retained so the
--                           dedup payload comparison and audit trail see
--                           what the caller actually requested.
-- orders.display_qty      — ICEBERG visible slice; also the
--                           iceberg_visible_qty audit field of
--                           Task 5.3.22. (Fills the schema gap left by
--                           the never-materialized migration 038 —
--                           spec §5.4 lists the column as canonical.)
-- orders.post_only /
--       reduce_only       — spec §5.4/§6.5 execution flags (same 038 gap).
-- orders.stp_mode         — spec §5.4 self-trade-prevention mode column;
--                           NULL = engine uses the account default
--                           (accounts.default_stp_mode, migration 094).
-- orders.session_id       — REST/WS cancel-on-disconnect attribution
--                           (Task 5.3.25 item 4): which session/key
--                           submitted the order so a session drop can
--                           mass-cancel exactly its own orders.
-- accounts.cancel_on_disconnect — account-level CoD flag, default false
--                           (spec §9.3/§24 #153 REST/WS semantics).

BEGIN;

ALTER TABLE orders
    ADD COLUMN order_seq        BIGINT       NOT NULL DEFAULT 0,
    ADD COLUMN quote_quantity   DECIMAL(28,8),
    ADD COLUMN display_qty      DECIMAL(28,8),
    ADD COLUMN post_only        BOOLEAN      NOT NULL DEFAULT FALSE,
    ADD COLUMN reduce_only      BOOLEAN      NOT NULL DEFAULT FALSE,
    ADD COLUMN stp_mode         VARCHAR(16),          -- NULL = account default (094)
    ADD COLUMN session_id       VARCHAR(64);

ALTER TABLE accounts
    ADD COLUMN cancel_on_disconnect BOOLEAN NOT NULL DEFAULT FALSE;

-- Hot path for the CoD/session mass-cancel sweep.
CREATE INDEX idx_orders_session_open
    ON orders (session_id)
    WHERE status IN ('PENDING','RESERVED','ACTIVE','PARTIALLY_FILLED');

COMMIT;
