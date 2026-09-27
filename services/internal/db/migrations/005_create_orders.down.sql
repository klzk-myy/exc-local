-- 005_create_orders.down.sql
BEGIN;

DROP TABLE IF EXISTS orders CASCADE;
DROP TYPE IF EXISTS order_status_enum;
DROP TYPE IF EXISTS time_in_force_enum;
DROP TYPE IF EXISTS order_type_enum;
DROP TYPE IF EXISTS order_side_enum;

COMMIT;
