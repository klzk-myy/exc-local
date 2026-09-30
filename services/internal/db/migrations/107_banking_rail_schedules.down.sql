-- 107_banking_rail_schedules — down
--
-- NOTE: PostgreSQL cannot drop a value from an enum type. Rolling back
-- removes the schedules table; 'QUEUED_FOR_NEXT_CYCLE' remains in
-- settlement_status_enum as an inert extra value (rows in that status are
-- removed first). This mirrors the codebase's additive-enum convention.

BEGIN;

UPDATE settlement_instructions
   SET status = 'PENDING'
 WHERE status = 'QUEUED_FOR_NEXT_CYCLE';

DROP TABLE IF EXISTS banking_rail_schedules;

COMMIT;
