-- 172_maintenance_windows.down.sql

BEGIN;

DROP TABLE IF EXISTS maintenance_windows;
DROP TYPE IF EXISTS maintenance_status_enum;
DROP TYPE IF EXISTS maintenance_scope_enum;

COMMIT;
