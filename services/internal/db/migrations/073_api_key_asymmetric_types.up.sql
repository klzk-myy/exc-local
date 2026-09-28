-- 073_api_key_asymmetric_types.up.sql
-- Phase-05 Task 5.3.38 — Ed25519/RSA programmatic API keys.
-- Spec §5.39: adds key_type, public_key, algorithm, rotates_from_id,
-- overlap_until to api_keys. Asymmetric public keys only — private keys
-- never enter the platform (§24 #283).
--
-- algorithm vocabulary: HMAC-SHA256 (legacy shared-secret), EdDSA,
-- RS256 (RSA PKCS#1 v1.5 + SHA-256), PS256 (RSA-PSS + SHA-256).

BEGIN;

CREATE TYPE api_key_type_enum AS ENUM ('HMAC', 'ED25519', 'RSA');

ALTER TABLE api_keys
    ADD COLUMN key_type        api_key_type_enum NOT NULL DEFAULT 'HMAC',
    ADD COLUMN public_key      BYTEA,             -- DER (PKIX) public key; NULL for HMAC
    ADD COLUMN algorithm       VARCHAR(16) NOT NULL DEFAULT 'HMAC-SHA256',
    ADD COLUMN rotates_from_id BIGINT REFERENCES api_keys (id),  -- rotation lineage
    ADD COLUMN overlap_until   TIMESTAMPTZ;       -- predecessor stays valid until this instant

-- Asymmetric keys must carry a public key; HMAC keys must not (shared
-- secret lives in secret_enc). CHECK fails closed on a mismatched pair.
ALTER TABLE api_keys
    ADD CONSTRAINT api_keys_key_material_consistent CHECK (
        (key_type = 'HMAC' AND public_key IS NULL) OR
        (key_type IN ('ED25519', 'RSA') AND public_key IS NOT NULL)
    ),
    ADD CONSTRAINT api_keys_algorithm_matches_type CHECK (
        (key_type = 'HMAC'    AND algorithm = 'HMAC-SHA256') OR
        (key_type = 'ED25519' AND algorithm = 'EdDSA') OR
        (key_type = 'RSA'     AND algorithm IN ('RS256', 'PS256'))
    );

COMMIT;
