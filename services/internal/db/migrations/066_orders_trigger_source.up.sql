-- 066_orders_trigger_source.up.sql
-- Phase-16 Task 16.3.17 — conditional/stop trigger-source column
-- (spec §6.2 remediation #12: dual-price trigger for conditional orders).
--
--   trigger_source  LAST_PRICE | MARK_PRICE | INDEX_PRICE — which price
--                   stream arms the stop/conditional trigger. LAST_PRICE
--                   is the baseline; MARK/INDEX ride the Phase-19.5
--                   oracle and are gated at admission on oracle freshness
--                   (CONDITIONAL_TRIGGER_ORACLE_STALE).
--
-- NOT NULL + DEFAULT keeps legacy rows honest — a NULL source would be
-- an ambiguous trigger state, so we tighten the spec'd shape with the
-- constraint rather than letting NULL mean "unspecified".

BEGIN;

ALTER TABLE orders
    ADD COLUMN trigger_source VARCHAR(16) NOT NULL DEFAULT 'LAST_PRICE'
        CHECK (trigger_source IN ('LAST_PRICE', 'MARK_PRICE', 'INDEX_PRICE'));

COMMIT;
