-- 236_liquidation_basis.up.sql
-- Phase-19.5 Task 19.5.3.6 step 5 (spec §24 #196).
--
-- liquidation_events.liquidation_basis records the pricing provenance
-- of every force-close leg: MARK for a fresh oracle mark, STALE_MARK
-- for a leg priced off the stale-price fallback ladder (last valid mark
-- ± tier haircut). Post-incident review filters on this flag; default
-- 'MARK' back-fills every pre-existing row honestly.
BEGIN;

ALTER TABLE liquidation_events
    ADD COLUMN liquidation_basis VARCHAR(16) NOT NULL DEFAULT 'MARK'
    CHECK (liquidation_basis IN ('MARK', 'STALE_MARK'));

CREATE INDEX liquidation_events_basis_idx
    ON liquidation_events (liquidation_basis, created_at DESC)
    WHERE liquidation_basis <> 'MARK';

COMMIT;
