-- 197_ops_status.down.sql
BEGIN;
DROP TABLE IF EXISTS ops_incidents;
DROP TABLE IF EXISTS ops_component_state;
DROP TABLE IF EXISTS ops_status_events;
COMMIT;
