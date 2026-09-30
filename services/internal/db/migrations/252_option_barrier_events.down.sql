-- 252_option_barrier_events.down.sql — drop the Task 22.3.5 knock log.

BEGIN;

DROP TABLE IF EXISTS option_barrier_events;

COMMIT;
