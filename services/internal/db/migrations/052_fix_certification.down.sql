-- 052_fix_certification.down.sql
-- Phase-18 Task 18.3.11 — drop the certification register and the
-- fix_sessions certificate-identity columns.

BEGIN;

ALTER TABLE fix_sessions
    DROP COLUMN IF EXISTS client_build,
    DROP COLUMN IF EXISTS environment,
    DROP COLUMN IF EXISTS cert_expires_at,
    DROP COLUMN IF EXISTS cert_rotation_ends_at,
    DROP COLUMN IF EXISTS cert_rollover_fingerprint,
    DROP COLUMN IF EXISTS cert_cn,
    DROP COLUMN IF EXISTS cert_fingerprint;

DROP TABLE IF EXISTS fix_certifications;

COMMIT;
