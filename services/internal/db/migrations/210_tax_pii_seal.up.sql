-- 210_tax_pii_seal.up.sql
-- PII-F1 remediation (Phase-13.5 audit finding): tax_self_certifications
-- carries GOV_ID-class PII — `tin` (SSN/EIN/ITIN) and the `fields` JSONB
-- document (legal_name/address/entity/treaty claims) — stored plaintext
-- since migration 205. This migration adds sealed mirrors written by the
-- compliance service (AES-256-GCM, nonce||ciphertext, auth.SecretBox over
-- secrets.data_key — the same envelope as api_keys.secret_enc).
--
-- The plaintext columns are KEPT for the read-fallback window: rows
-- inserted before this migration still have tin/fields populated and
-- NULL sealed columns until the app-layer backfill runs:
--
--     exchange seal-tax-pii            # seals legacy rows, NULLs plaintext
--
-- Crypto lives in the app layer (SecretBox), so SQL cannot migrate the
-- data — the CLI must run after this migration. Once it completes, every
-- row has tin IS NULL and fields = '{}'::jsonb.

BEGIN;

ALTER TABLE tax_self_certifications
    ADD COLUMN tin_sealed    BYTEA,   -- AES-256-GCM nonce‖ct of the TIN
    ADD COLUMN fields_sealed BYTEA;   -- AES-256-GCM nonce‖ct of the fields JSON document

COMMENT ON COLUMN tax_self_certifications.tin IS
    'DEPRECATED plaintext (PII-F1) — read-fallback during the migration '
    'window only; service writes NULL and backfill NULLs legacy rows. '
    'Sealed copy in tin_sealed.';
COMMENT ON COLUMN tax_self_certifications.fields IS
    'DEPRECATED plaintext (PII-F1) — read-fallback during the migration '
    'window only; service writes ''{}'' and backfill resets legacy rows. '
    'Sealed copy in fields_sealed.';
COMMENT ON COLUMN tax_self_certifications.tin_sealed IS
    'AES-256-GCM sealed TIN (auth.SecretBox, secrets.data_key; Vault/KMS in production)';
COMMENT ON COLUMN tax_self_certifications.fields_sealed IS
    'AES-256-GCM sealed fields JSON document (legal_name/address/entity/treaty claims)';

COMMIT;
