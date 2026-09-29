-- 075_opo_order_lists.down.sql — reverse Task 16.3.20 schema.

BEGIN;

DROP TABLE IF EXISTS order_list_legs;
DROP TABLE IF EXISTS order_lists;
DROP TYPE IF EXISTS order_list_state_enum;
DROP TYPE IF EXISTS contingency_type_enum;

COMMIT;
