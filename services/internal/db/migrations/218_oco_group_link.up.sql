-- 218_oco_group_link.up.sql
-- Phase-14 Task 14.3.1 — OCO (one-cancels-other) pair linkage (spec §6.2,
-- §6.5, §24 #47; column reserved by 005_create_orders).
--
--   oco_group_id  links the two legs of an OCO pair — both rows share one
--                 group id minted at submit time (the same value the
--                 matching engine journals as WalOcoLinkPayload.link_id).
--                 NULL = standalone order. When one leg reaches terminal
--                 FILLED the engine cancels the sibling with
--                 kWalCancelReasonOcoLink (7) and the read model records
--                 the reason via an order_audit OCO_SIBLING_CANCEL row.
--                 Phase-16 bracket/OTO (Task 16.3.14) reuses the column
--                 for parent→children linkage.
--
-- Partial index only — the overwhelmingly common NULL case stays out of
-- the index; pair lookups scan exactly two rows.

BEGIN;

ALTER TABLE orders
    ADD COLUMN oco_group_id BIGINT;

CREATE INDEX idx_orders_oco_group
    ON orders (oco_group_id)
    WHERE oco_group_id IS NOT NULL;

COMMIT;
