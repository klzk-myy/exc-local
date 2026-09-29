-- 030_create_fix_sessions.down.sql

BEGIN;

DROP TABLE IF EXISTS fix_messages;
DROP TABLE IF EXISTS fix_sessions;
DROP TYPE IF EXISTS fix_session_status_enum;

COMMIT;
