-- 072_execution_rules_stp_groups.down.sql
BEGIN;

DROP TABLE IF EXISTS prevented_matches;

ALTER TABLE orders
    DROP COLUMN IF EXISTS expiry_reason,
    DROP COLUMN IF EXISTS prevented_qty;

ALTER TABLE accounts DROP COLUMN IF EXISTS trade_group_id;

ALTER TABLE instruments DROP COLUMN IF EXISTS execution_rule;

COMMIT;
