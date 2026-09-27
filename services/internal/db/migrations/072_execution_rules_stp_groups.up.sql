-- 072_execution_rules_stp_groups.up.sql
-- Spec §5.39 (remediation #14): reference-price execution configuration and
-- immutable STP (self-trade prevention) non-trade records.
--   instruments.execution_rule  — per-instrument execution-rule config (JSONB)
--   accounts.trade_group_id     — STP group membership
--   orders.expiry_reason        — persisted expiry/collar reason
--                                 (Phase-02 Task 2.3.17)
--   orders.prevented_qty        — qty suppressed by STP (Task 2.3.18)
--   prevented_matches           — immutable audit rows for STP non-trades

BEGIN;

ALTER TABLE instruments
    ADD COLUMN execution_rule JSONB;

ALTER TABLE accounts
    ADD COLUMN trade_group_id BIGINT;

ALTER TABLE orders
    ADD COLUMN expiry_reason  VARCHAR(64),
    ADD COLUMN prevented_qty  DECIMAL(28,8) NOT NULL DEFAULT 0;

CREATE TABLE prevented_matches (
    id                  BIGSERIAL PRIMARY KEY,
    symbol              VARCHAR(20) NOT NULL,
    maker_order_id      BIGINT NOT NULL,
    taker_order_id      BIGINT NOT NULL,
    trade_group_id      BIGINT,
    mode                VARCHAR(32) NOT NULL,
    price               DECIMAL(20,8),
    maker_prevented_qty DECIMAL(28,8),
    taker_prevented_qty DECIMAL(28,8),
    transact_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
