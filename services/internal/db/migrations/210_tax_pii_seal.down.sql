-- 210_tax_pii_seal.down.sql
-- PRIVACY REGRESSION WARNING: dropping the sealed columns reverts to the
-- plaintext-only schema of migration 205 — PII-F1 reopens. Before running
-- this down migration, restore the plaintext columns via the app layer
-- (crypto cannot run in SQL):
--
--     exchange seal-tax-pii --restore  # unseals tin_sealed/fields_sealed
--                                      # back into tin/fields
--
-- Rows sealed after 210 and never restored lose tin/fields irrecoverably
-- once these columns drop (the sealed blobs are the only copy).

BEGIN;

ALTER TABLE tax_self_certifications
    DROP COLUMN IF EXISTS tin_sealed,
    DROP COLUMN IF EXISTS fields_sealed;

COMMENT ON COLUMN tax_self_certifications.tin IS
    'raw TIN as submitted';
COMMENT ON COLUMN tax_self_certifications.fields IS
    'legal name, address, entity type, treaty claims';

COMMIT;
