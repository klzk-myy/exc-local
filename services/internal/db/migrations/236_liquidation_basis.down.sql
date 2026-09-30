-- 236_liquidation_basis.down.sql
BEGIN;

DROP INDEX IF EXISTS liquidation_events_basis_idx;
ALTER TABLE liquidation_events DROP COLUMN IF EXISTS liquidation_basis;

COMMIT;
