-- 106_positions_isolated_margin.up.sql
-- Phase-19 Task 19.3.27 (spec §13.15, §24 #411): position-level isolated
-- margin sub-allocation.
--
--   isolated_margin_allocated — collateral dedicated to this position and
--     locked out of the account's general available balance. Denominated
--     in the account's base currency (USD numeraire, spec §13.1). Only an
--     ISOLATED-mode position carries a non-zero value; the isolated
--     liquidation path may consume at most this amount plus the
--     position's unrealized P&L — never the wider account equity.
--   auto_margin_replenish — when true the risk engine tops the
--     allocation back to the maintenance level from balances.available
--     before dispatching the isolated leg to liquidation (bounded by
--     available balance; fails closed, never partial phantom margin).
--
-- Columns pre-announced in migration 014's header (positions baseline).

BEGIN;

ALTER TABLE positions
    ADD COLUMN IF NOT EXISTS isolated_margin_allocated DECIMAL(28,8) NOT NULL DEFAULT 0.0
        CHECK (isolated_margin_allocated >= 0);

ALTER TABLE positions
    ADD COLUMN IF NOT EXISTS auto_margin_replenish BOOLEAN NOT NULL DEFAULT FALSE;

COMMIT;
