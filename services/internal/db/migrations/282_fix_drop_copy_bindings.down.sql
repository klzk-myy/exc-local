-- 282_fix_drop_copy_bindings.down.sql
BEGIN;

DROP TABLE IF EXISTS fix_cert_revocations;
DROP TABLE IF EXISTS fix_session_bindings;

COMMIT;
