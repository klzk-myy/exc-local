-- 276_deferred_variation_margin_journal_fk.down.sql — drop the deferred FK.

BEGIN;

ALTER TABLE IF EXISTS variation_margin
    DROP CONSTRAINT IF EXISTS variation_margin_journal_fk;

COMMIT;
