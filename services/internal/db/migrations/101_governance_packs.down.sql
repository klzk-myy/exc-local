-- 101_governance_packs.down.sql

BEGIN;

DROP TRIGGER IF EXISTS trg_governance_packs_immutable ON governance_packs;
DROP FUNCTION IF EXISTS governance_packs_released_immutable();
DROP TABLE IF EXISTS governance_packs;

COMMIT;
