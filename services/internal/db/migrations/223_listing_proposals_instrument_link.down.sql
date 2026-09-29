-- 223_listing_proposals_instrument_link.down.sql
-- Reverses 223: drops the listing_proposals review/dual-control/activation
-- columns and the ops-queue index.

BEGIN;

DROP INDEX IF EXISTS listing_proposals_status_idx;

ALTER TABLE listing_proposals
    DROP COLUMN IF EXISTS activate_at,
    DROP COLUMN IF EXISTS instrument_id,
    DROP COLUMN IF EXISTS dual_control_id,
    DROP COLUMN IF EXISTS reviewed_at,
    DROP COLUMN IF EXISTS reviewed_by;

COMMIT;
