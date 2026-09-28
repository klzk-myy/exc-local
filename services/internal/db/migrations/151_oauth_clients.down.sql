-- 151_oauth_clients.down.sql
BEGIN;

DROP TABLE IF EXISTS oauth_clients CASCADE;
DROP TYPE IF EXISTS oauth_client_status_enum;

COMMIT;
