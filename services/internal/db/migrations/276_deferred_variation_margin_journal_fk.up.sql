-- 276_deferred_variation_margin_journal_fk.up.sql
-- Deferred foreign key: variation_margin.journal_entry_id -> journal_entries.
--
-- variation_margin (034) declares journal_entry_id without the FK clause
-- because it is numbered ahead of 036_create_general_ledger (journal_entries).
-- The constraint is NOT applied inside 036: several test fixtures apply a
-- hardcoded migration subset that includes 036 but not 034, and any ALTER on
-- variation_margin there fails (or silently hits public.* under a composite
-- search_path). A trailing file is in no subset and runs only in the full
-- ordered apply.

BEGIN;

ALTER TABLE variation_margin
    ADD CONSTRAINT variation_margin_journal_fk
    FOREIGN KEY (journal_entry_id) REFERENCES journal_entries (id);

COMMIT;
