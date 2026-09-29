-- 047_otr_limits.down.sql
-- Phase-13 Task 13.3.6 — drop the RTS-9 OTR columns added by 047.

BEGIN;

ALTER TABLE risk_limits
    DROP COLUMN IF EXISTS otr_window;

ALTER TABLE risk_limits
    DROP COLUMN IF EXISTS max_order_to_trade_ratio;

COMMIT;
