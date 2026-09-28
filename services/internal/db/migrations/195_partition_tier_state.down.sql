-- 195_partition_tier_state.down.sql
BEGIN;

DROP TABLE IF EXISTS partition_tier_log;
DROP TABLE IF EXISTS partition_tier_state;

COMMIT;
