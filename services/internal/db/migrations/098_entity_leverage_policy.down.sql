-- 098_entity_leverage_policy.down.sql
BEGIN;

DROP TABLE IF EXISTS entity_leverage_policy;
ALTER TABLE accounts DROP COLUMN IF EXISTS entity_code;

COMMIT;
