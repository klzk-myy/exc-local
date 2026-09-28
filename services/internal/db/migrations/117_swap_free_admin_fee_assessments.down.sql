-- 114_swap_free_admin_fee_assessments.down.sql

BEGIN;

DROP TABLE IF EXISTS swap_free_admin_fee_assessments;
DROP TYPE IF EXISTS swap_free_fee_status_enum;

COMMIT;
