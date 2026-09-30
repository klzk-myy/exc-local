-- 233_accounts_position_mode.down.sql
-- Reverses the hedging-capable uniqueness. HEDGING accounts may hold
-- LONG+SHORT rows on one instrument that cannot survive the restored
-- (account, instrument) unique index — for each such pair the row with
-- the smaller id is folded away (dev/test rollback only; production
-- rollbacks must flatten hedged positions first).
BEGIN;

-- Drop coexistent duplicate (account_id, instrument_id) rows keeping
-- the newest id, so the net invariant can be restored.
DELETE FROM positions a
USING positions b
WHERE a.account_id = b.account_id
  AND a.instrument_id = b.instrument_id
  AND a.side <> b.side
  AND a.id < b.id;

DROP INDEX IF EXISTS uq_positions_account_instrument_side;
CREATE UNIQUE INDEX IF NOT EXISTS uq_positions_account_instrument
    ON positions (account_id, instrument_id);

ALTER TABLE accounts DROP COLUMN IF EXISTS position_mode;
DROP TYPE IF EXISTS position_mode_enum;

COMMIT;
