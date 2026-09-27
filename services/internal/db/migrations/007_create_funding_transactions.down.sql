-- 007_create_funding_transactions.down.sql
BEGIN;

DROP TABLE IF EXISTS funding_transactions CASCADE;
DROP TYPE IF EXISTS bank_method_enum;
DROP TYPE IF EXISTS funding_status_enum;
DROP TYPE IF EXISTS funding_type_enum;

COMMIT;
