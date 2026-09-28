-- 154_order_idempotency.down.sql
BEGIN;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS client_order_id_dedup;
COMMIT;
