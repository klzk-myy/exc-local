-- 228_fixsbe_sessions.down.sql — Phase-18 SBE gateway tables.

BEGIN;

DROP TABLE IF EXISTS fixsbe_schema_registry;
DROP TABLE IF EXISTS fixsbe_sessions;

COMMIT;
