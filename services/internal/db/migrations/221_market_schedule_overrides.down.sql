-- 221_market_schedule_overrides.down.sql — drop the Phase-15 Task
-- 15.3.4 schedule-override store. The `market:hours` Redis projection is
-- republished canonical-only after this lands (the service's boot
-- reconcile rewrites the key from whatever PG holds).

BEGIN;

DROP TABLE IF EXISTS market_schedule_overrides;

COMMIT;
