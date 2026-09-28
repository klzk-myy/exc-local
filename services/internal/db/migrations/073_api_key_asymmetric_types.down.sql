-- 073_api_key_asymmetric_types.down.sql
BEGIN;

ALTER TABLE api_keys
    DROP CONSTRAINT IF EXISTS api_keys_algorithm_matches_type,
    DROP CONSTRAINT IF EXISTS api_keys_key_material_consistent,
    DROP COLUMN IF EXISTS overlap_until,
    DROP COLUMN IF EXISTS rotates_from_id,
    DROP COLUMN IF EXISTS algorithm,
    DROP COLUMN IF EXISTS public_key,
    DROP COLUMN IF EXISTS key_type;

DROP TYPE IF EXISTS api_key_type_enum;

COMMIT;
