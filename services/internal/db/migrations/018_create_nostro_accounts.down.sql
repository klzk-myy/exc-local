-- 018_create_nostro_accounts.down.sql
BEGIN;

DROP TABLE IF EXISTS nostro_accounts CASCADE;
DROP TYPE IF EXISTS nostro_status_enum;

COMMIT;
