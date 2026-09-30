-- 233_accounts_position_mode.up.sql
-- Phase-19 Task 19.3.15 (spec §13.6e, §24 #229).
--
-- 1. position_mode_enum + accounts.position_mode — NETTING (ESMA retail
--    default) or HEDGING (professional/institutional). Toggle is allowed
--    only while the account holds zero open positions (§13.6e item 3).
--
-- 2. Positions uniqueness relatches from (account_id, instrument_id) to
--    (account_id, instrument_id, side) — supersedes the migration-111
--    uq_positions_account_instrument net-position invariant for hedging
--    accounts: a LONG and a SHORT row may coexist on the same instrument
--    only when the account is in HEDGING mode. NETTING accounts still
--    keep a single non-flat row per instrument by service contract (the
--    fill path consumes the opposing row before opening the same-side
--    row); flat (quantity=0) rows on both sides are harmless residue.

BEGIN;

CREATE TYPE position_mode_enum AS ENUM ('NETTING', 'HEDGING');

ALTER TABLE accounts
    ADD COLUMN position_mode position_mode_enum NOT NULL DEFAULT 'NETTING';

-- Index swap inside the transaction: existing data is one row per
-- (account, instrument) so the wider key cannot conflict.
DROP INDEX IF EXISTS uq_positions_account_instrument;
CREATE UNIQUE INDEX uq_positions_account_instrument_side
    ON positions (account_id, instrument_id, side);

COMMIT;
