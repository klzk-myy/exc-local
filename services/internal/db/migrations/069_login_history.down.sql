-- 069_login_history.down.sql

BEGIN;

DROP INDEX IF EXISTS idx_login_history_user_ts;
DROP TABLE IF EXISTS login_history;
DROP TYPE IF EXISTS login_result_enum;

COMMIT;
