-- 010_create_admin_audit_log.down.sql
BEGIN;

DROP TABLE IF EXISTS admin_audit_log CASCADE;

COMMIT;
