-- 070_users_anti_phishing_code.down.sql

BEGIN;

ALTER TABLE users
    DROP CONSTRAINT IF EXISTS users_anti_phishing_code_len;

ALTER TABLE users
    DROP COLUMN IF EXISTS anti_phishing_code;

COMMIT;
