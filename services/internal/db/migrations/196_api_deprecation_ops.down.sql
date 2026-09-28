-- 196_api_deprecation_ops.down.sql
BEGIN;
DROP TABLE IF EXISTS api_deprecation_hits;
ALTER TABLE api_deprecations
    DROP COLUMN IF EXISTS sunset_processed_at,
    DROP COLUMN IF EXISTS status;
COMMIT;
