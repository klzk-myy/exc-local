-- 155_order_pipeline_columns.down.sql
BEGIN;
DROP INDEX IF EXISTS idx_orders_session_open;
ALTER TABLE accounts DROP COLUMN IF EXISTS cancel_on_disconnect;
ALTER TABLE orders
    DROP COLUMN IF EXISTS session_id,
    DROP COLUMN IF EXISTS stp_mode,
    DROP COLUMN IF EXISTS reduce_only,
    DROP COLUMN IF EXISTS post_only,
    DROP COLUMN IF EXISTS display_qty,
    DROP COLUMN IF EXISTS quote_quantity,
    DROP COLUMN IF EXISTS order_seq;
COMMIT;
