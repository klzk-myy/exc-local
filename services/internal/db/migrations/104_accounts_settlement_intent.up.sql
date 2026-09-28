-- 104_accounts_settlement_intent.up.sql
-- Phase-03 Task 3.3.22 (spec §5.45.2 / §17.4a, §24 #406): physical-delivery
-- vs rolling-margin ledger partitioning.
--
-- settlement_intent marks whether an account's (or an individual order's)
-- spot flow is PHYSICAL_DELIVERY (corporate deliverable FX — settled over
-- banking rails, EXEMPT from Tom-Next rollover financing) or ROLLING_MARGIN
-- (leveraged rolling spot — the default CFD-style treatment that accrues
-- daily swap points at the 17:00 ET roll).
--
-- orders.settlement_intent is NULL-able: NULL inherits accounts.settlement_intent
-- ("defaults to account setting", spec §5.45.2 / accounts row §5.2) — an
-- explicit value is a per-order override.
--
-- The Task-3.3.22 header note "migrations 103 and 104" was written before
-- 103 was reassigned to orders.discretionary_offset_pips; per spec §5.45.2
-- BOTH columns are owned by migration 104 and are created here.

BEGIN;

CREATE TYPE settlement_intent_enum AS ENUM ('PHYSICAL_DELIVERY', 'ROLLING_MARGIN');

ALTER TABLE accounts
    ADD COLUMN settlement_intent settlement_intent_enum NOT NULL DEFAULT 'ROLLING_MARGIN';

ALTER TABLE orders
    ADD COLUMN settlement_intent settlement_intent_enum;   -- NULL = inherit account intent

COMMIT;
