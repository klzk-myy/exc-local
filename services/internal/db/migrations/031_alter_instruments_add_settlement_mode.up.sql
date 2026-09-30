-- 031_alter_instruments_add_settlement_mode.up.sql
-- Phase-19 Task 19.3.6 (spec §5.1 `settlement_mode`, §24 #98).
--
-- instruments.settlement_mode selects how the settlement pipeline emits
-- payment instructions for the instrument:
--
--   GROSS — each trade settles independently (full notional per leg); the
--           per-leg SWIFT/pacs.009 dispatch path of Phase-03 Task 3.3.3.
--   NET   — trades between the same counterparty pair net to a single net
--           obligation per currency per settlement_date; the
--           GrossNetService batching path in internal/settlement emits one
--           payment per (account, counterparty, currency, value date).
--
-- Column default is 'GROSS' (the conservative, always-correct mode — a
-- NET default would silently net obligations for instruments nobody
-- audited). Per Phase-19 Task 19.3.6 step 4 the type-level default is
-- then applied to existing rows: FORWARD and SWAP instruments settle NET,
-- everything else (SPOT, NDF, OPTION) stays GROSS. New rows inherit the
-- 'GROSS' column default; instrument onboarding (Phase-15 maker-checker,
-- Task 15.3.8) sets 'NET' explicitly for forwards/swaps — the mode is
-- admin-set per instrument, never derived at read time.
--
-- Pre-announced by migration 001's header (settlement_mode owned here).

BEGIN;

CREATE TYPE settlement_mode_enum AS ENUM ('GROSS', 'NET');

ALTER TABLE instruments
    ADD COLUMN settlement_mode settlement_mode_enum NOT NULL DEFAULT 'GROSS';

UPDATE instruments
   SET settlement_mode = 'NET'
 WHERE instrument_type IN ('FORWARD', 'SWAP');

COMMIT;
