-- 042_client_categorization.down.sql
-- Reverts Phase-14 Task 14.3.7: drops the assessment table and the
-- accounts columns, then the enum type (created by the up migration).

BEGIN;

DROP TABLE IF EXISTS appropriateness_assessments;

ALTER TABLE accounts
    DROP COLUMN IF EXISTS nbp,
    DROP COLUMN IF EXISTS client_category;

DROP TYPE IF EXISTS client_category_enum;

COMMIT;
