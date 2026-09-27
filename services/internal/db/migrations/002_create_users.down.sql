-- 002_create_users.down.sql
BEGIN;

DROP TABLE IF EXISTS users CASCADE;
DROP TYPE IF EXISTS kyc_status_enum;
DROP TYPE IF EXISTS user_status_enum;

COMMIT;
