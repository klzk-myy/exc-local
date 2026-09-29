-- 225_bracket_orders.down.sql — reverse Task 16.3.14 schema.

BEGIN;

DROP TABLE IF EXISTS bracket_children;
DROP TABLE IF EXISTS bracket_orders;
DROP TYPE IF EXISTS bracket_state_enum;

COMMIT;
