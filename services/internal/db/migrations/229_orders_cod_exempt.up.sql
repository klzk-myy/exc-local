-- 229_orders_cod_exempt.up.sql
-- Phase-18 Task 18.3.18 (spec §5.4 order column catalog, remediation #35;
-- §9.9 Cancel-on-Disconnect semantics): per-order exemption from the
-- cancel-on-disconnect purge. An order marked cod_exempt survives the
-- session-loss sweep (spec: "excluding orders explicitly marked
-- COD_EXEMPT") — dead-man, admin, close-all and every other mass-cancel
-- reason still take it; the exemption is scoped to the
-- cancel_on_disconnect sweep only (PgStore.OpenOrders).
-- FIX-side marking: venue tag 9510 (COD_EXEMPT) on 35=D.
ALTER TABLE orders
    ADD COLUMN cod_exempt BOOLEAN NOT NULL DEFAULT FALSE;
