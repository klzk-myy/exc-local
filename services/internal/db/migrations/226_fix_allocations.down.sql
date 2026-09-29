-- 226_fix_allocations.down.sql — drops Phase-18 Task 18.3.13 FIX allocation schema.

BEGIN;

DROP TRIGGER IF EXISTS allocation_events_no_update ON allocation_events;
DROP FUNCTION IF EXISTS allocation_events_immutable;
DROP TABLE IF EXISTS allocation_events;
DROP TABLE IF EXISTS fix_allocation_legs;
DROP TABLE IF EXISTS fix_allocations;
DROP TYPE IF EXISTS alloc_leg_status_enum;
DROP TYPE IF EXISTS fix_alloc_status_enum;
DROP TYPE IF EXISTS fix_alloc_method_enum;

COMMIT;
