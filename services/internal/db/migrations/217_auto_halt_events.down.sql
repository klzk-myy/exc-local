-- 217_auto_halt_events.down.sql — drop the Phase-14 auto-halt audit
-- table. Circuit-breaker state transitions (migration 206) are
-- untouched; only the detector-attribution layer is removed.

BEGIN;

DROP TABLE IF EXISTS auto_halt_events;

COMMIT;
