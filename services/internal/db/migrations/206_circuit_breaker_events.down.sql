-- 206_circuit_breaker_events.down.sql — drops the Phase-13 Task 13.3.1/
-- 13.3.9 breaker audit table and its enums.

BEGIN;

DROP TABLE IF EXISTS circuit_breaker_events;
DROP TYPE IF EXISTS circuit_breaker_state_enum;
DROP TYPE IF EXISTS circuit_breaker_scope_enum;

COMMIT;
