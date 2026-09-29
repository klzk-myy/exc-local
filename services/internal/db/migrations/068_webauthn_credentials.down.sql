-- 068_webauthn_credentials.down.sql

BEGIN;

DROP INDEX IF EXISTS idx_webauthn_credentials_user;
DROP TABLE IF EXISTS webauthn_credentials;

COMMIT;
