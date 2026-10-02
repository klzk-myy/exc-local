DROP INDEX IF EXISTS idx_orders_gtd_expire_at;
ALTER TABLE orders DROP COLUMN IF EXISTS gtd_expire_at;
